package wiki.hubu.rocwol.data.journal

import android.content.ClipData
import android.content.Context
import android.content.Intent
import androidx.core.content.FileProvider
import androidx.room.Room
import androidx.room.withTransaction
import java.io.File
import java.time.Instant
import java.util.UUID
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import wiki.hubu.rocwol.data.AppMigration

/**
 * Private, no-backup local log. record() confirms queue acceptance only; recordConfirmed()
 * confirms a Room transaction. Neither an in-memory queue nor a submitted wake implies success.
 * This service never mutates identity, authorization or the remembered wake state.
 */
class OperationJournal private constructor(context: Context) {
    private val context = context.applicationContext
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val commands = Channel<Command>(128)
    private val mutableRecords = MutableStateFlow<List<JournalRecord>>(emptyList())
    val records: StateFlow<List<JournalRecord>> = mutableRecords.asStateFlow()
    private val mutableHealth = MutableStateFlow(JournalHealth())
    val health: StateFlow<JournalHealth> = mutableHealth.asStateFlow()
    private var database: JournalDatabase? = null

    init {
        scope.launch {
            try {
                val db = database()
                db.withTransaction {
                    db.events().expire(System.currentTimeMillis() - RETENTION_MS)
                    db.events().trim()
                }
                cleanupExports()
            } catch (_: Exception) {
                storageFailure()
            }
            for (command in commands) {
                try {
                    val db = database()
                    when (command) {
                        is Command.Append -> {
                            db.withTransaction {
                                val dao = db.events()
                                dao.expire(System.currentTimeMillis() - RETENTION_MS)
                                val previous = if (command.event.aggregates) dao.latest(command.event.scopeKey()) else null
                                if (previous != null && previous.trigger == JournalTrigger.AUTOMATIC.name &&
                                    previous.fingerprint == command.event.fingerprint() &&
                                    command.timestamp - previous.lastTimestampMs in 0..AGGREGATION_WINDOW_MS) {
                                    dao.aggregate(previous.id, command.timestamp, command.event.durationMs)
                                } else {
                                    dao.insert(StoredEvent.from(command.event, command.timestamp))
                                }
                                dao.trim()
                            }
                            mutableHealth.update { it.copy(lastConfirmedWrite = Instant.now(), problem = JournalProblem.NONE) }
                            command.reply?.complete(true)
                        }
                        is Command.Clear -> {
                            db.events().clear()
                            mutableRecords.value = emptyList()
                            cleanupExports(deleteAll = true)
                            command.reply.complete(true)
                        }
                        is Command.Export -> {
                            db.events().expire(System.currentTimeMillis() - RETENTION_MS)
                            command.reply.complete(export(db.events().snapshot().mapNotNull { it.record() }))
                        }
                        Command.Prune -> db.events().expire(System.currentTimeMillis() - RETENTION_MS)
                    }
                } catch (_: Exception) {
                    if (command is Command.Export) {
                        mutableHealth.update { it.copy(problem = JournalProblem.EXPORT_FAILED) }
                    } else storageFailure()
                    when (command) {
                        is Command.Append -> {
                            mutableHealth.update { it.copy(droppedRecords = it.droppedRecords + 1) }
                            command.reply?.complete(false)
                        }
                        is Command.Clear -> command.reply.complete(false)
                        is Command.Export -> command.reply.complete(null)
                        Command.Prune -> Unit
                    }
                } finally {
                    if (command is Command.Append) mutableHealth.update { it.copy(pendingWrites = (it.pendingWrites - 1).coerceAtLeast(0)) }
                }
            }
        }
        scope.launch {
            records.collectLatest { current ->
                val oldest = current.minOfOrNull { it.timestamp.toEpochMilli() } ?: return@collectLatest
                delay((oldest + RETENTION_MS + 1 - System.currentTimeMillis()).coerceAtLeast(1))
                while (!commands.trySend(Command.Prune).isSuccess) delay(100)
            }
        }
    }

    /** Nonblocking; false means rejected or full. A true return does not promise durable storage. */
    fun record(event: JournalEvent): Boolean = submit(event, null)

    /** Waits for the ordered write transaction. Invalid input/storage failures return false. */
    suspend fun recordConfirmed(event: JournalEvent): Boolean {
        val reply = CompletableDeferred<Boolean>()
        return if (submit(event, reply)) reply.await() else false
    }

    /** Queue-ordered barrier: earlier accepted events cannot reappear after clearing. */
    suspend fun clear(): Boolean {
        val reply = CompletableDeferred<Boolean>()
        if (!enqueueControl(Command.Clear(reply))) return false
        return reply.await()
    }

    /** Returns a grant-read-only Sharesheet intent, or null on a safe export failure. */
    suspend fun exportShareIntent(): Intent? {
        val reply = CompletableDeferred<Intent?>()
        if (!enqueueControl(Command.Export(reply))) return null
        return reply.await()
    }

    private fun submit(event: JournalEvent, reply: CompletableDeferred<Boolean>?): Boolean {
        if (!event.isSafeToAppend()) {
            mutableHealth.update { it.copy(problem = JournalProblem.INVALID_EVENT, droppedRecords = it.droppedRecords + 1) }
            return false
        }
        mutableHealth.update { it.copy(pendingWrites = it.pendingWrites + 1) }
        if (commands.trySend(Command.Append(event.localOnly(), System.currentTimeMillis(), reply)).isSuccess) return true
        mutableHealth.update { it.copy(problem = JournalProblem.QUEUE_FULL, pendingWrites = (it.pendingWrites - 1).coerceAtLeast(0), droppedRecords = it.droppedRecords + 1) }
        return false
    }

    // Never retain unbounded suspended senders outside the bounded app-wide command queue.
    private fun enqueueControl(command: Command): Boolean {
        if (commands.trySend(command).isSuccess) return true
        mutableHealth.update { it.copy(problem = JournalProblem.QUEUE_FULL) }
        return false
    }

    // Called exclusively by the IO command consumer; opening the database never blocks the UI.
    private suspend fun database(): JournalDatabase {
        AppMigration.ensure(context)
        database?.let { return it }
        val db = openRaw(context)
        database = db
        scope.launch {
            try {
                db.events().observe().collect { stored ->
                    val cutoff = System.currentTimeMillis() - RETENTION_MS
                    mutableRecords.value = stored.filter { it.timestampMs >= cutoff }.mapNotNull { it.record() }
                    mutableHealth.update { it.copy(loaded = true) }
                }
            } catch (_: Exception) {
                storageFailure()
            }
        }
        return db
    }

    private fun storageFailure() {
        mutableHealth.update { it.copy(loaded = true, problem = JournalProblem.STORAGE_UNAVAILABLE) }
    }

    private fun export(events: List<JournalRecord>): Intent {
        val directory = File(context.cacheDir, "log-exports")
        check(directory.isDirectory || directory.mkdirs())
        cleanupExports(maxFiles = 2)
        val file = File(directory, "operation-log-${UUID.randomUUID()}.txt")
        try {
            file.outputStream().bufferedWriter(Charsets.UTF_8).use { writer ->
                writer.appendLine("网络唤醒 · 本机操作日志")
                writer.appendLine("仅含本机保留的最近 7 天、最多 2000 条记录。重复自动检查会合并。")
                writer.appendLine("已发送不代表电脑已开机；在线状态是探测结果，不证明由本次唤醒导致。")
                writer.appendLine("不含凭据、服务器/电脑名称、域名、原始标识、IP/MAC 或请求/响应正文。")
                writer.appendLine("服务器、电脑和线路使用本份导出内稳定的本地别名；请求 ID 和受控错误类别可用于排障。")
                writer.appendLine()
                val aliases = JournalAliases()
                events.forEach { writer.appendLine(it.safeText(aliases)); writer.appendLine() }
            }
            val uri = FileProvider.getUriForFile(context, "wiki.hubu.rocwol.logs", file)
            val send = Intent(Intent.ACTION_SEND).apply {
                type = "text/plain"
                putExtra(Intent.EXTRA_STREAM, uri)
                clipData = ClipData.newRawUri("本机操作日志", uri)
                addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
            }
            return Intent.createChooser(send, "分享脱敏日志").apply {
                addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
            }
        } catch (failure: Exception) {
            file.delete()
            throw failure
        }
    }

    private fun cleanupExports(deleteAll: Boolean = false, maxFiles: Int = 3) {
        val directory = File(context.cacheDir, "log-exports")
        val files = directory.listFiles()?.filter { it.isFile && it.name.startsWith("operation-log-") && it.extension == "txt" }
            ?.sortedByDescending { it.lastModified() } ?: return
        val cutoff = System.currentTimeMillis() - EXPORT_TTL_MS
        files.forEachIndexed { index, file ->
            if (deleteAll || index >= maxFiles || file.lastModified() < cutoff) check(file.delete())
        }
    }

    private sealed interface Command {
        data class Append(val event: JournalEvent, val timestamp: Long, val reply: CompletableDeferred<Boolean>?) : Command
        data class Clear(val reply: CompletableDeferred<Boolean>) : Command
        data class Export(val reply: CompletableDeferred<Intent?>) : Command
        data object Prune : Command
    }

    companion object {
        private const val RETENTION_MS = 7 * 24 * 60 * 60 * 1000L
        private const val AGGREGATION_WINDOW_MS = 15 * 60 * 1000L
        private const val EXPORT_TTL_MS = 24 * 60 * 60 * 1000L
        @Volatile private var instance: OperationJournal? = null
        suspend fun ready(context: Context): OperationJournal {
            AppMigration.ensure(context)
            return get(context)
        }

        /** Migration barrier's raw entry point. Must not call ready/get/ensure here. */
        suspend fun migrateStorage(context: Context) = withContext(Dispatchers.IO) {
            openRaw(context.applicationContext).close()
        }

        private fun openRaw(context: Context): JournalDatabase {
            val file = File(context.noBackupFilesDir, "operation-journal.db")
            val db = Room.databaseBuilder(context, JournalDatabase::class.java, file.absolutePath)
                .addMigrations(JournalDatabase.MIGRATION_1_2)
                .build()
            try {
                db.openHelper.writableDatabase
            } catch (failure: Exception) {
                db.close()
                throw failure
            }
            return db
        }

        fun get(context: Context): OperationJournal = instance ?: synchronized(this) {
            instance ?: OperationJournal(context).also { instance = it }
        }
    }
}
