package wiki.hubu.rocwol.data.journal

import android.content.ContentValues
import android.database.sqlite.SQLiteDatabase
import androidx.room.Room
import androidx.room.withTransaction
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import java.util.UUID
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.runBlocking
import org.junit.Assert.*
import org.junit.Test
import org.junit.runner.RunWith
import wiki.hubu.rocwol.data.OperationContext

@RunWith(AndroidJUnit4::class)
class JournalMigrationTest {
    private val context get() = InstrumentationRegistry.getInstrumentation().targetContext

    private fun open(name: String) = Room.databaseBuilder(context, JournalDatabase::class.java, name)
        .addMigrations(JournalDatabase.MIGRATION_1_2).build()

    @Test fun realV1RowsMigrateWithoutLosingLegacyOrUnknownRecords() = runBlocking(Dispatchers.IO) {
        val name = "journal-migration-${UUID.randomUUID()}.db"
        val file = context.getDatabasePath(name)
        file.parentFile!!.mkdirs()
        SQLiteDatabase.openOrCreateDatabase(file, null).use { old ->
            old.execSQL(V1_SCHEMA)
            old.execSQL("CREATE INDEX index_operation_events_timestampMs ON operation_events(timestampMs)")
            old.execSQL("CREATE INDEX index_operation_events_scopeKey ON operation_events(scopeKey)")
            old.version = 1
            old.execSQL("CREATE TABLE room_master_table (id INTEGER PRIMARY KEY, identity_hash TEXT)")
            old.execSQL("INSERT INTO room_master_table (id, identity_hash) VALUES (42, '24da47e824ed93b37063775c808b2cc9')")
            insertV1(old, 1, "https://legacy-one.example", "DESKTOP")
            insertV1(old, 2, null, "DESKTOP")
            insertV1(old, 3, "https://legacy-two.example", "UNKNOWN_OLD_TARGET")
            insertV1(old, 4, null, "UNKNOWN_OLD_TARGET")
            insertV1(old, 5, null, null)
            insertV1(old, 6, "", "UNKNOWN_OLD_TARGET")
            insertV1(old, 7, null, "desktop")
        }
        try {
            var db = open(name)
            try {
                val rows = db.events().snapshot().associateBy { it.id }
                assertEquals((1L..7L).toSet(), rows.keys)
                assertEquals(7, rowCount(db))
                assertTrue(rows.values.all { it.record() != null })
                assertEquals(LEGACY_JOURNAL_PROFILE_ID, rows.getValue(1).profileId)
                assertEquals(LEGACY_JOURNAL_PROFILE_ID, rows.getValue(2).profileId)
                assertEquals(LEGACY_JOURNAL_PROFILE_ID, rows.getValue(3).profileId)
                assertEquals("desktop", rows.getValue(1).targetId)
                assertEquals("desktop", rows.getValue(2).targetId)
                assertNull(rows.getValue(3).targetId)
                assertNull(rows.getValue(4).profileId)
                assertNull(rows.getValue(4).targetId)
                assertNull(rows.getValue(5).profileId)
                assertNull(rows.getValue(6).profileId)
                assertNull(rows.getValue(7).profileId)
                assertNull(rows.getValue(7).targetId)
                assertTrue(rows.values.all { it.routeIndex == null })
                assertEquals("UNKNOWN_OLD_TARGET", rows.getValue(4).target)
                assertEquals("https://legacy-one.example", rows.getValue(1).origin)
                assertEquals(2, rows.getValue(1).occurrences)
                assertEquals(2000L, rows.getValue(1).lastTimestampMs)
                assertTrue(rows.values.all { it.timestampMs == 1000L && it.lastTimestampMs == 2000L && it.occurrences == 2 })
                assertTrue(rows.values.all { it.requestId == "a".repeat(32) && it.fingerprint == "legacy-fingerprint" })
                assertEquals("WAKE_POLL|$LEGACY_JOURNAL_PROFILE_ID|desktop||${"a".repeat(32)}", rows.getValue(1).scopeKey)
                assertTrue(rows.values.none { it.scopeKey.contains("https://") })
                val aliases = JournalAliases()
                val export = rows.values.joinToString("\n") { it.record()!!.safeText(aliases) }
                assertFalse(export.contains("legacy-one.example"))
                assertFalse(export.contains("legacy-two.example"))
                assertFalse(export.contains("UNKNOWN_OLD_TARGET"))
                assertFalse(export.contains(LEGACY_JOURNAL_PROFILE_ID))
                assertTrue(export.contains("未知目标"))
                assertTrue(export.contains("旧记录 / 未知服务器"))
                val fresh = JournalEvent(JournalAction.TARGET_CHECK, JournalOutcome.SUCCESS,
                    context = OperationContext("11111111-1111-1111-1111-111111111111", "https://private-new.example",
                        targetId = "private-new-pc", routeIndex = 2))
                db.events().insert(StoredEvent.from(fresh, 3000))
                assertEquals(8, rowCount(db))
                val newest = db.events().snapshot().first()
                assertNull(newest.origin)
                assertNull(newest.target)
                assertEquals("private-new-pc", newest.targetId)
                assertEquals(2, newest.routeIndex)
            } finally { db.close() }
            db = open(name)
            try {
                val rows = db.events().snapshot()
                assertEquals(8, rows.size)
                assertEquals(7, rows.count { it.id in 1L..7L })
                assertEquals(LEGACY_JOURNAL_PROFILE_ID, rows.first { it.id == 1L }.profileId)
            } finally { db.close() }
        } finally { context.deleteDatabase(name) }
    }

    @Test fun retentionAndCapacityAreGlobalAcrossProfiles() = runBlocking(Dispatchers.IO) {
        val name = "journal-limits-${UUID.randomUUID()}.db"
        val db = open(name)
        try {
            val now = System.currentTimeMillis()
            db.withTransaction {
                repeat(2005) { index ->
                    val profile = if (index % 2 == 0) "11111111-1111-1111-1111-111111111111" else "22222222-2222-2222-2222-222222222222"
                    val event = JournalEvent(JournalAction.SETTINGS_CHANGE, JournalOutcome.SUCCESS,
                        context = OperationContext(profile, "", routeIndex = 1))
                    db.events().insert(StoredEvent.from(event, now + index))
                }
                db.events().trim()
            }
            assertEquals(2000, db.events().snapshot().size)
            assertEquals(2000, rowCount(db))
            assertEquals(2, db.events().snapshot().map { it.profileId }.toSet().size)
            val old = JournalEvent(JournalAction.SETTINGS_CHANGE, JournalOutcome.SUCCESS,
                context = OperationContext(LEGACY_JOURNAL_PROFILE_ID, ""))
            db.events().insert(StoredEvent.from(old, now - 8 * 24 * 60 * 60 * 1000L))
            db.events().expire(now - 7 * 24 * 60 * 60 * 1000L)
            assertEquals(2000, db.events().snapshot().size)
            assertEquals(2000, rowCount(db))
            assertTrue(db.events().snapshot().none { it.profileId == LEGACY_JOURNAL_PROFILE_ID })
        } finally {
            db.close()
            context.deleteDatabase(name)
        }
    }

    @Test fun identicalRequestsAndAutomaticChecksStayScopedInRoom() = runBlocking(Dispatchers.IO) {
        val name = "journal-scopes-${UUID.randomUUID()}.db"
        val db = open(name)
        try {
            val serverA = "11111111-1111-1111-1111-111111111111"
            val serverB = "22222222-2222-2222-2222-222222222222"
            val base = JournalEvent(JournalAction.WAKE_POLL, JournalOutcome.PENDING,
                trigger = JournalTrigger.AUTOMATIC,
                context = OperationContext(serverA, "", targetId = "pc-a", routeIndex = 1),
                requestId = "a".repeat(32), wakeStage = WakeStage.PENDING)
            val events = listOf(base,
                base.copy(context = base.context!!.copy(profileId = serverB)),
                base.copy(context = base.context!!.copy(targetId = "pc-b")),
                base.copy(context = base.context!!.copy(routeIndex = 2)))
            db.withTransaction {
                events.forEach { db.events().insert(StoredEvent.from(it, 1000)) }
                val first = db.events().latest(base.scopeKey())!!
                db.events().aggregate(first.id, 2000, 42)
            }
            val rows = db.events().snapshot()
            assertEquals(4, rowCount(db))
            assertEquals(3, rows.map { it.record()!!.historyKey }.toSet().size)
            events.forEach { event ->
                val row = db.events().latest(event.scopeKey())!!
                assertEquals(event.context!!.profileId, row.profileId)
                assertEquals(event.context.targetId, row.targetId)
                assertEquals(event.context.routeIndex, row.routeIndex)
                assertEquals(if (event == base) 2 else 1, row.occurrences)
            }
        } finally {
            db.close()
            context.deleteDatabase(name)
        }
    }

    private fun rowCount(db: JournalDatabase): Int =
        db.openHelper.writableDatabase.query("SELECT COUNT(*) FROM operation_events").use { cursor ->
            check(cursor.moveToFirst())
            cursor.getInt(0)
        }

    private fun insertV1(db: SQLiteDatabase, id: Long, origin: String?, target: String?) {
        val values = ContentValues().apply {
            put("id", id)
            put("timestampMs", 1000L)
            put("lastTimestampMs", 2000L)
            put("occurrences", 2)
            put("action", "WAKE_POLL")
            put("outcome", "PENDING")
            put("trigger", "AUTOMATIC")
            put("origin", origin)
            put("target", target)
            put("requestId", "a".repeat(32))
            put("wakeStage", "PENDING")
            put("scopeKey", "WAKE_POLL|${origin.orEmpty()}|${"a".repeat(32)}")
            put("fingerprint", "legacy-fingerprint")
        }
        assertTrue(db.insertOrThrow("operation_events", null, values) > 0)
    }

    companion object {
        // Exact exported Room v1 column types/defaults; creating it does not involve v2 entities.
        private const val V1_SCHEMA = "CREATE TABLE operation_events (" +
            "id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL, timestampMs INTEGER NOT NULL, " +
            "action TEXT NOT NULL, outcome TEXT NOT NULL, trigger TEXT NOT NULL, origin TEXT, requestId TEXT, " +
            "httpStatus INTEGER, error TEXT, durationMs INTEGER, wakeStage TEXT, target TEXT, reachability TEXT, " +
            "router TEXT, authorizationMinutes INTEGER, allowAllDevices INTEGER, scopeKey TEXT NOT NULL, " +
            "fingerprint TEXT NOT NULL, occurrences INTEGER NOT NULL DEFAULT 1, lastTimestampMs INTEGER NOT NULL DEFAULT 0)"
    }
}
