package wiki.hubu.rocwol.data.journal

import androidx.room.Dao
import androidx.room.Database
import androidx.room.Entity
import androidx.room.Index
import androidx.room.Insert
import androidx.room.PrimaryKey
import androidx.room.Query
import androidx.room.RoomDatabase
import kotlinx.coroutines.flow.Flow
import androidx.room.migration.Migration
import androidx.sqlite.db.SupportSQLiteDatabase
import wiki.hubu.rocwol.data.OperationContext
import java.time.Instant

@Entity(tableName = "operation_events", indices = [Index("timestampMs"), Index("scopeKey")])
internal data class StoredEvent(
    @PrimaryKey(autoGenerate = true) val id: Long = 0,
    val timestampMs: Long,
    val action: String,
    val outcome: String,
    val trigger: String,
    val origin: String?,
    val requestId: String?,
    val httpStatus: Int?,
    val error: String?,
    val durationMs: Long?,
    val wakeStage: String?,
    val target: String?,
    val reachability: String?,
    val router: String?,
    val authorizationMinutes: Int?,
    val allowAllDevices: Boolean?,
    val scopeKey: String,
    val fingerprint: String,
    @androidx.room.ColumnInfo(defaultValue = "1") val occurrences: Int = 1,
    @androidx.room.ColumnInfo(defaultValue = "0") val lastTimestampMs: Long = 0,
    val profileId: String? = null,
    val targetId: String? = null,
    val routeIndex: Int? = null
) {
    fun record(): JournalRecord? = runCatching {
        if (profileId == null && (targetId != null || routeIndex != null)) return null
        val event = JournalEvent(
            action = JournalAction.valueOf(action), outcome = JournalOutcome.valueOf(outcome),
            trigger = JournalTrigger.valueOf(trigger),
            context = profileId?.let { OperationContext(it, "", targetId = targetId, routeIndex = routeIndex) },
            requestId = requestId,
            httpStatus = httpStatus, error = error?.let { value -> JournalError.entries.firstOrNull { it.name == value } ?: JournalError.OTHER },
            durationMs = durationMs, wakeStage = wakeStage?.let(WakeStage::valueOf),
            reachability = reachability?.let(JournalReachability::valueOf), router = router?.let(JournalRouter::valueOf),
            authorizationMinutes = authorizationMinutes, allowAllDevices = allowAllDevices
        )
        if (!event.isSafe() || occurrences !in 1..1_000_000) return null
        JournalRecord(id, Instant.ofEpochMilli(timestampMs), Instant.ofEpochMilli(lastTimestampMs), occurrences, event, legacyOrigin = origin)
    }.getOrNull()

    companion object {
        fun from(event: JournalEvent, now: Long) = StoredEvent(
            timestampMs = now, action = event.action.name, outcome = event.outcome.name, trigger = event.trigger.name,
            origin = null, requestId = event.requestId, httpStatus = event.httpStatus, error = event.error?.name,
            durationMs = event.durationMs, wakeStage = event.wakeStage?.name, target = null,
            reachability = event.reachability?.name, router = event.router?.name,
            authorizationMinutes = event.authorizationMinutes, allowAllDevices = event.allowAllDevices,
            scopeKey = event.scopeKey(), fingerprint = event.fingerprint(), lastTimestampMs = now,
            profileId = event.context?.profileId, targetId = event.context?.targetId, routeIndex = event.context?.routeIndex
        )
    }
}

@Dao
internal interface JournalDao {
    @Insert suspend fun insert(event: StoredEvent)

    @Query("SELECT * FROM operation_events ORDER BY timestampMs DESC, id DESC LIMIT 2000")
    fun observe(): Flow<List<StoredEvent>>

    @Query("SELECT * FROM operation_events ORDER BY timestampMs DESC, id DESC LIMIT 2000")
    suspend fun snapshot(): List<StoredEvent>

    @Query("SELECT * FROM operation_events WHERE scopeKey = :scope ORDER BY id DESC LIMIT 1")
    suspend fun latest(scope: String): StoredEvent?

    @Query("UPDATE operation_events SET occurrences = MIN(occurrences + 1, 1000000), lastTimestampMs = :now, durationMs = :duration WHERE id = :id")
    suspend fun aggregate(id: Long, now: Long, duration: Long?)

    @Query("DELETE FROM operation_events WHERE timestampMs < :cutoff")
    suspend fun expire(cutoff: Long)

    @Query("DELETE FROM operation_events WHERE id NOT IN (SELECT id FROM operation_events ORDER BY timestampMs DESC, id DESC LIMIT 2000)")
    suspend fun trim()

    @Query("DELETE FROM operation_events")
    suspend fun clear()
}

@Database(entities = [StoredEvent::class], version = 2, exportSchema = true)
internal abstract class JournalDatabase : RoomDatabase() {
    abstract fun events(): JournalDao

    companion object {
        /** Keep every v1 row and its legacy columns; never destructively recreate the log. */
        val MIGRATION_1_2 = object : Migration(1, 2) {
            override fun migrate(db: SupportSQLiteDatabase) {
                db.execSQL("ALTER TABLE operation_events ADD COLUMN profileId TEXT")
                db.execSQL("ALTER TABLE operation_events ADD COLUMN targetId TEXT")
                db.execSQL("ALTER TABLE operation_events ADD COLUMN routeIndex INTEGER")
                db.execSQL(
                    "UPDATE operation_events SET profileId = ? WHERE (origin IS NOT NULL AND origin != '') OR target = 'DESKTOP'",
                    arrayOf(LEGACY_JOURNAL_PROFILE_ID)
                )
                db.execSQL("UPDATE operation_events SET targetId = 'desktop' WHERE target = 'DESKTOP'")
                db.execSQL(
                    "UPDATE operation_events SET scopeKey = action || '|' || COALESCE(profileId, '') || '|' || " +
                        "COALESCE(targetId, '') || '||' || COALESCE(requestId, '')"
                )
            }
        }
    }
}
