package wiki.hubu.rocwol.reminders

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import androidx.work.CoroutineWorker
import androidx.work.ExistingWorkPolicy
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import androidx.work.workDataOf
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import org.json.JSONObject
import wiki.hubu.rocwol.MainActivity
import wiki.hubu.rocwol.R
import wiki.hubu.rocwol.data.ManagementAuthorization
import java.util.UUID
import java.util.concurrent.TimeUnit

class AuthorizationReminders private constructor(context: Context) {
    private val app = context.applicationContext
    private val preferences = app.getSharedPreferences("authorization_reminders", Context.MODE_PRIVATE)
    private val notifications = requireNotNull(app.getSystemService(NotificationManager::class.java))
    private val work by lazy { WorkManager.getInstance(app) }
    private val lock = Mutex()
    private val mutableEnabled = MutableStateFlow(preferences.getBoolean("enabled", false))
    val enabled: StateFlow<Boolean> = mutableEnabled.asStateFlow()

    init {
        ensureChannel()
        // Repair a process exit between committing the snapshot and enqueueing its persisted work.
        CoroutineScope(SupervisorJob() + Dispatchers.IO).launch {
            lock.withLock { reconcile(readSnapshot()) }
        }
    }

    suspend fun setEnabled(value: Boolean) = withContext(Dispatchers.IO) {
        lock.withLock {
            check(preferences.edit().putBoolean("enabled", value).commit())
            mutableEnabled.value = value
            reconcile(readSnapshot())
        }
    }

    /** Unknown/offline or pending changes never establish that a confirmed grant was revoked. */
    suspend fun update(
        grant: ManagementAuthorization?,
        known: Boolean,
        pending: Boolean,
        replace: Boolean = false
    ) = withContext(Dispatchers.IO) {
        lock.withLock {
            val previous = readSnapshot()
            if (grant == null) {
                if (known && !pending && previous != null) clearLocked()
                return@withLock
            }
            // Unconfirmed refreshes cannot replace a newer snapshot, even with stale cached data.
            if (!known && previous != null) {
                return@withLock
            }
            val expiry = if (grant.isPermanent) null else grant.expiresAt.toEpochMilli()
            val sameGrant = previous != null && previous.origin == grant.origin &&
                previous.sessionMinutes == grant.sessionMinutes &&
                previous.allDevices == grant.allowAllDevices && previous.expiresAt == expiry
            if (sameGrant && !replace) {
                return@withLock
            }
            val now = System.currentTimeMillis()
            val snapshot = ReminderSnapshot(
                generation = UUID.randomUUID().toString(),
                origin = grant.origin,
                sessionMinutes = grant.sessionMinutes,
                allDevices = grant.allowAllDevices,
                expiresAt = expiry,
                remindAt = ReminderPolicy.remindAt(now, grant.allowAllDevices, expiry)
            )
            saveSnapshot(snapshot)
            notifications.cancel(NOTIFICATION_ID)
            work.cancelAllWorkByTag(WORK_NAME).result.get()
            reconcile(snapshot)
        }
    }

    /** Use only for confirmed revocation or local identity removal, not network failures. */
    suspend fun clear() = withContext(Dispatchers.IO) { lock.withLock { clearLocked() } }

    enum class NotificationAvailability { AVAILABLE, PERMISSION_REQUIRED, APP_BLOCKED, CHANNEL_BLOCKED }

    fun notificationAvailability(): NotificationAvailability {
        ensureChannel()
        if (Build.VERSION.SDK_INT >= 33 &&
            app.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) return NotificationAvailability.PERMISSION_REQUIRED
        if (!notifications.areNotificationsEnabled()) return NotificationAvailability.APP_BLOCKED
        if (notifications.getNotificationChannel(CHANNEL_ID)?.importance == NotificationManager.IMPORTANCE_NONE) {
            return NotificationAvailability.CHANNEL_BLOCKED
        }
        return NotificationAvailability.AVAILABLE
    }

    internal suspend fun deliver(generation: String) = withContext(Dispatchers.IO) {
        lock.withLock {
            val snapshot = readSnapshot() ?: return@withLock
            if (!snapshot.mayNotify(generation, preferences.getBoolean("enabled", false), System.currentTimeMillis())) {
                return@withLock
            }
            if (notificationAvailability() != NotificationAvailability.AVAILABLE) return@withLock
            val intent = Intent(app, MainActivity::class.java)
                .putExtra(OPEN_MANAGEMENT_EXTRA, true)
                .addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP or Intent.FLAG_ACTIVITY_SINGLE_TOP)
            val pendingIntent = PendingIntent.getActivity(
                app, NOTIFICATION_ID, intent, PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
            )
            val publicContent = Notification.Builder(app, CHANNEL_ID)
                .setSmallIcon(R.drawable.ic_authorization_reminder)
                .setContentTitle("网络唤醒")
                .setContentText("有一项安全提醒，请打开 App 查看")
                .setContentIntent(pendingIntent)
                .build()
            val message = "上次确认的管理授权可能仍然开放，请打开 App 核实或取消。此提醒未连接服务器检查当前状态。"
            val notification = Notification.Builder(app, CHANNEL_ID)
                .setSmallIcon(R.drawable.ic_authorization_reminder)
                .setContentTitle("请核实管理授权")
                .setContentText("上次确认的授权可能仍然开放，请打开 App 核实或取消")
                .setStyle(Notification.BigTextStyle().bigText(message))
                .setContentIntent(pendingIntent)
                .setAutoCancel(true)
                .setOnlyAlertOnce(true)
                .setCategory(Notification.CATEGORY_REMINDER)
                .setVisibility(Notification.VISIBILITY_PRIVATE)
                .setPublicVersion(publicContent)
                .setTimeoutAfter(snapshot.expiresAt?.let { (it - System.currentTimeMillis()).coerceAtLeast(1) } ?: 0)
                .build()
            // Commit before posting: an interrupted worker cannot produce a duplicate alert.
            saveSnapshot(snapshot.copy(notified = true))
            try {
                notifications.notify(NOTIFICATION_ID, notification)
            } catch (_: SecurityException) {
                // Permission can be revoked between the check and posting. No retry loop.
                saveSnapshot(snapshot)
            }
        }
    }

    private fun clearLocked() {
        check(preferences.edit().remove("snapshot").commit())
        notifications.cancel(NOTIFICATION_ID)
        work.cancelAllWorkByTag(WORK_NAME).result.get()
    }

    private fun reconcile(snapshot: ReminderSnapshot?) {
        val now = System.currentTimeMillis()
        if (!preferences.getBoolean("enabled", false) || snapshot == null || snapshot.notified ||
            snapshot.remindAt == null || (snapshot.expiresAt != null && now >= snapshot.expiresAt)
        ) {
            work.cancelAllWorkByTag(WORK_NAME).result.get()
            if (!preferences.getBoolean("enabled", false) || snapshot == null ||
                (snapshot.expiresAt != null && now >= snapshot.expiresAt)
            ) notifications.cancel(NOTIFICATION_ID)
            return
        }
        if (notificationAvailability() != NotificationAvailability.AVAILABLE) {
            work.cancelAllWorkByTag(WORK_NAME).result.get()
            return
        }
        val request = OneTimeWorkRequestBuilder<AuthorizationReminderWorker>()
            .setInputData(workDataOf(GENERATION_KEY to snapshot.generation))
            .addTag(WORK_NAME)
            .setInitialDelay((snapshot.remindAt - now).coerceAtLeast(0), TimeUnit.MILLISECONDS)
            .build()
        // A previous generation cannot occupy the new grant's slot after a process exit.
        // KEEP retains this generation's persisted absolute deadline on ordinary restarts.
        work.enqueueUniqueWork("$WORK_NAME:${snapshot.generation}", ExistingWorkPolicy.KEEP, request).result.get()
    }

    private fun ensureChannel() {
        notifications.createNotificationChannel(
            NotificationChannel(CHANNEL_ID, "管理授权安全提醒", NotificationManager.IMPORTANCE_DEFAULT).apply {
                description = "可选的本地授权检查提醒，不会自动授权、取消授权或连接服务器"
                lockscreenVisibility = Notification.VISIBILITY_PRIVATE
            }
        )
    }

    private fun readSnapshot(): ReminderSnapshot? = try {
        preferences.getString("snapshot", null)?.let { value ->
            val json = JSONObject(value)
            ReminderSnapshot(
                generation = json.getString("generation"),
                origin = json.getString("origin"),
                sessionMinutes = json.getInt("sessionMinutes"),
                allDevices = json.getBoolean("allDevices"),
                expiresAt = if (json.isNull("expiresAt")) null else json.getLong("expiresAt"),
                remindAt = if (json.isNull("remindAt")) null else json.getLong("remindAt"),
                notified = json.getBoolean("notified")
            )
        }
    } catch (_: Exception) { null }

    private fun saveSnapshot(snapshot: ReminderSnapshot) {
        val json = JSONObject()
            .put("generation", snapshot.generation)
            .put("origin", snapshot.origin)
            .put("sessionMinutes", snapshot.sessionMinutes)
            .put("allDevices", snapshot.allDevices)
            .put("expiresAt", snapshot.expiresAt ?: JSONObject.NULL)
            .put("remindAt", snapshot.remindAt ?: JSONObject.NULL)
            .put("notified", snapshot.notified)
        check(preferences.edit().putString("snapshot", json.toString()).commit())
    }

    companion object {
        const val CHANNEL_ID = "authorization_reminders"
        const val OPEN_MANAGEMENT_EXTRA = "open_management"
        internal const val GENERATION_KEY = "generation"
        private const val WORK_NAME = "authorization-reminder"
        private const val NOTIFICATION_ID = 4101
        @Volatile private var instance: AuthorizationReminders? = null

        fun get(context: Context): AuthorizationReminders = instance ?: synchronized(this) {
            instance ?: AuthorizationReminders(context).also { instance = it }
        }
    }
}

class AuthorizationReminderWorker(context: Context, parameters: WorkerParameters) : CoroutineWorker(context, parameters) {
    override suspend fun doWork(): Result {
        val generation = inputData.getString(AuthorizationReminders.GENERATION_KEY) ?: return Result.success()
        AuthorizationReminders.get(applicationContext).deliver(generation)
        return Result.success()
    }
}
