package wiki.hubu.rocwol.reminders

import android.Manifest
import android.app.NotificationManager
import android.content.Context
import android.os.Build
import android.os.SystemClock
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import kotlinx.coroutines.runBlocking
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test
import org.junit.runner.RunWith
import wiki.hubu.rocwol.data.AppMigration
import wiki.hubu.rocwol.data.ServerOrigin
import wiki.hubu.rocwol.data.ServerProfile
import wiki.hubu.rocwol.data.ServerProfiles
import java.util.UUID

@RunWith(AndroidJUnit4::class)
class ReminderIsolationTest {
    @Test fun deliveringAndClearingOneServerCannotReplaceAnotherServersNotification() = runBlocking {
        val instrumentation = InstrumentationRegistry.getInstrumentation()
        val app = instrumentation.targetContext
        if (Build.VERSION.SDK_INT >= 33) {
            instrumentation.uiAutomation.grantRuntimePermission(app.packageName, Manifest.permission.POST_NOTIFICATIONS)
        }
        AppMigration.ensure(app)
        val profiles = ServerProfiles.get(app)
        val reminders = AuthorizationReminders.get(app)
        val prefs = app.getSharedPreferences("authorization_reminders", Context.MODE_PRIVATE)
        val notifications = requireNotNull(app.getSystemService(NotificationManager::class.java))
        val previouslyEnabled = prefs.getBoolean("enabled", false)
        val ids = List(2) { UUID.randomUUID().toString() }
        val generations = List(2) { UUID.randomUUID().toString() }
        try {
            ids.forEachIndexed { index, id ->
                val origin = "https://reminder-$index.example"
                val instance = (index + 1).toString().repeat(32)
                profiles.installLegacy(ServerProfile(id, "Reminder fixture", instance,
                    listOf(ServerOrigin(origin, 1, instance)), 2, origin, null,
                    "roc_wol_${id}_wake_v1", "roc_wol_${id}_manage_v1"))
            }
            reminders.setEnabled(true)
            ids.forEachIndexed { index, id ->
                val snapshot = JSONObject().put("generation", generations[index])
                    .put("origin", "https://reminder-$index.example").put("sessionMinutes", 0)
                    .put("allDevices", false).put("expiresAt", JSONObject.NULL)
                    .put("remindAt", System.currentTimeMillis() - 1000).put("notified", false)
                check(prefs.edit().putString("snapshot:$id", snapshot.toString()).commit())
                reminders.deliver(id, generations[index])
            }
            fun activeIds() = notifications.activeNotifications.filter { it.id == 4101 }.map { it.tag }.toSet()
            suspend fun awaitActiveIds(expected: Set<String>) {
                val deadline = SystemClock.elapsedRealtime() + 5000
                while (activeIds().filter { it in ids }.toSet() != expected && SystemClock.elapsedRealtime() < deadline) {
                    kotlinx.coroutines.delay(25)
                }
                assertEquals(expected, activeIds().filter { it in ids }.toSet())
            }
            awaitActiveIds(ids.toSet())
            reminders.clear(ids[0])
            awaitActiveIds(setOf(ids[1]))
            assertFalse(activeIds().contains(ids[0]))
            assertTrue(activeIds().contains(ids[1]))
            reminders.deliver(ids[0], generations[0])
            reminders.deliver(ids[1], UUID.randomUUID().toString())
            assertFalse(activeIds().contains(ids[0]))
            assertTrue(activeIds().contains(ids[1]))
            assertTrue(JSONObject(requireNotNull(prefs.getString("snapshot:${ids[1]}", null))).getBoolean("notified"))
        } finally {
            ids.forEach { id -> reminders.clear(id); if (profiles.get(id) != null) profiles.remove(id, "profile") }
            reminders.setEnabled(previouslyEnabled)
        }
    }
}
