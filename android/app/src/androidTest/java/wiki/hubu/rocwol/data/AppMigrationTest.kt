package wiki.hubu.rocwol.data

import android.content.Context
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import kotlinx.coroutines.runBlocking
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import java.time.Instant

/** Run on a clean, task-owned emulator. Refuses to overwrite an existing installation. */
@RunWith(AndroidJUnit4::class)
class AppMigrationTest {
    @Test fun legacyIdentityWithMissingOriginIsRetainedOfflineAndReplayDoesNotOverwriteIt() = runBlocking {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        val source = context.getSharedPreferences("roc_wol_identity", Context.MODE_PRIVATE)
        val servers = context.getSharedPreferences("roc_wol_servers", Context.MODE_PRIVATE)
        val stage = context.getSharedPreferences("roc_wol_migration", Context.MODE_PRIVATE)
        check(source.all.isEmpty() && servers.all.isEmpty() && stage.all.isEmpty()) {
            "Migration test requires a clean task-owned application data directory"
        }
        val id = "0123456789abcdef0123456789abcdef"
        val wake = JSONObject().put("requestId", id).put("result", "unknown")
            .put("createdAt", Instant.now().toString()).put("gatewayConfirmed", false)
        check(source.edit().putString("deviceId", id).putString("deviceName", "旧手机")
            .putString("lastWake", wake.toString()).putBoolean("managementPending", true)
            .putInt("sessionMinutes", 15).putBoolean("autoOpenBrowser", false).commit())

        AppMigration.ensure(context)
        val profile = requireNotNull(ServerProfiles.get(context).get(LEGACY_PROFILE_ID))
        assertTrue(profile.origins.isEmpty())
        assertNull(profile.instanceId)
        assertNull(profile.selectedOrigin)
        assertEquals(LEGACY_WAKE_ALIAS, profile.wakeAlias)
        assertEquals(LEGACY_MANAGE_ALIAS, profile.manageAlias)
        assertEquals(LEGACY_PROFILE_ID, ServerProfiles.get(context).activeProfileId.value)
        val copied = context.getSharedPreferences("roc_wol_identity_$LEGACY_PROFILE_ID", Context.MODE_PRIVATE)
        assertEquals(id, copied.getString("deviceId", null))
        assertEquals(15, copied.getInt("sessionMinutes", -1))
        assertFalse(copied.getBoolean("autoOpenBrowser", true))
        assertTrue(copied.getBoolean("managementPending", false))
        val restored = JSONObject(requireNotNull(copied.getString("lastWake:desktop", null)))
        assertEquals("desktop", restored.getString("targetId"))
        assertEquals(id, restored.getString("requestId"))
        assertTrue(source.all.isEmpty())
        assertTrue(stage.getBoolean("complete_v2", false))

        check(copied.edit().putString("deviceName", "升级后修改").commit())
        AppMigration.ensure(context)
        assertEquals("升级后修改", copied.getString("deviceName", null))
        assertEquals(1, ServerProfiles.get(context).profiles.value.size)
        assertFalse(runCatching { ServerRepositories.get(context).get(LEGACY_PROFILE_ID).origin }.isSuccess)

        val repositories = ServerRepositories.get(context)
        val original = repositories.get(LEGACY_PROFILE_ID)
        val profiles = ServerProfiles.get(context)
        profiles.remove(LEGACY_PROFILE_ID, "identity")
        assertTrue(copied.all.isEmpty())
        assertNull(requireNotNull(profiles.get(LEGACY_PROFILE_ID)).removalMode)
        assertEquals(LEGACY_PROFILE_ID, profiles.activeProfileId.value)
        assertFalse(runCatching { original.withOperation { Unit } }.isSuccess)
        val replacement = repositories.get(LEGACY_PROFILE_ID)
        assertTrue(original !== replacement)
        assertNull(replacement.deviceId)
        AppMigration.ensure(context)
        assertTrue(copied.all.isEmpty())
        profiles.remove(LEGACY_PROFILE_ID, "profile")
        assertNull(profiles.get(LEGACY_PROFILE_ID))
        assertNull(profiles.activeProfileId.value)
    }
}
