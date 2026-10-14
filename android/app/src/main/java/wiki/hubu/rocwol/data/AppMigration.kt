package wiki.hubu.rocwol.data

import android.content.Context
import android.content.SharedPreferences
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import org.json.JSONObject
import wiki.hubu.rocwol.data.journal.OperationJournal
import wiki.hubu.rocwol.reminders.AuthorizationReminders
import wiki.hubu.rocwol.security.DeviceKeys

/** Shared initialization barrier. Each durable stage is idempotent; failures retain source data. */
object AppMigration {
    private val lock = Mutex()
    @Volatile private var ready = false
    suspend fun ensure(context: Context) {
        if (ready) return
        lock.withLock {
            if (ready) return@withLock
            withContext(NonCancellable + Dispatchers.IO) {
                val app = context.applicationContext
                val profiles = ServerProfiles.get(app)
                profiles.load()
                profiles.profiles.value.filter { it.removalMode != null }.forEach { finishRemoval(app, it.profileId) }
                val stage = app.getSharedPreferences("roc_wol_migration", Context.MODE_PRIVATE)
                if (!stage.getBoolean("complete_v2", false)) {
                    val old = app.getSharedPreferences("roc_wol_identity", Context.MODE_PRIVATE)
                    val reminders = app.getSharedPreferences("authorization_reminders", Context.MODE_PRIVATE)
                    val keys = DeviceKeys(LEGACY_WAKE_ALIAS, LEGACY_MANAGE_ALIAS)
                    val hasLegacy = old.contains("deviceId") || old.contains("lastWake") || old.contains("managementAuthorization") ||
                        old.getBoolean("managementPending", false) || keys.hasAnyIdentity() || reminders.contains("snapshot")
                    if (hasLegacy) {
                        val normalizedOrigin = old.getString("origin", null)?.let { value -> runCatching { Origins.checked(value) }.getOrNull() }
                        profiles.installLegacy(ServerProfile(LEGACY_PROFILE_ID, "原有服务器", null,
                            normalizedOrigin?.let { listOf(ServerOrigin(it, 1, null)) } ?: emptyList(),
                            if (normalizedOrigin == null) 1 else 2, null, null, LEGACY_WAKE_ALIAS, LEGACY_MANAGE_ALIAS))
                        val destination = app.getSharedPreferences("roc_wol_identity_$LEGACY_PROFILE_ID", Context.MODE_PRIVATE)
                        if (!destination.getBoolean("legacy_copy_complete", false)) {
                            check(destination.all.isEmpty()) { "旧身份迁移遇到已有未完成数据" }
                            val editor = destination.edit()
                            old.all.forEach { (key, value) ->
                                if (key !in setOf("origin", "lastWake", "managementAuthorization")) copy(editor, key, value)
                            }
                            old.getString("deviceId", null)?.let { require(it.matches(Regex("[0-9a-f]{32}"))) }
                            old.getString("lastWake", null)?.let { raw ->
                                val json = JSONObject(raw).put("targetId", "desktop")
                                WakeRecord.parse(json)
                                editor.putString("lastWake:desktop", json.toString())
                            }
                            old.getString("managementAuthorization", null)?.let { raw ->
                                val json = JSONObject(raw)
                                val origin = Origins.checked(json.getString("origin"))
                                json.put("origin", origin)
                                ManagementAuthorization.parse(json, listOf(origin))
                                editor.putString("managementAuthorization", json.toString())
                                editor.putBoolean("managementPending", true)
                            }
                            // Commit data and its stage together; replay never overwrites post-upgrade edits.
                            check(editor.putBoolean("legacy_copy_complete", true).commit()) { "旧身份迁移保存失败" }
                        }
                    }
                    OperationJournal.migrateStorage(app)
                    AuthorizationReminders.migrateLegacy(app)
                    check(stage.edit().putBoolean("complete_v2", true).commit()) { "迁移完成标记保存失败" }
                    check(old.edit().clear().commit()) { "旧身份迁移清理失败" }
                } else {
                    // Completion can precede source cleanup if a process exits between the two commits.
                    check(app.getSharedPreferences("roc_wol_identity", Context.MODE_PRIVATE).edit().clear().commit())
                }
                ready = true
            }
        }
    }
    private fun copy(editor: SharedPreferences.Editor, key: String, value: Any?) {
        when (value) {
            is String -> editor.putString(key, value)
            is Boolean -> editor.putBoolean(key, value)
            is Int -> editor.putInt(key, value)
            is Long -> editor.putLong(key, value)
            is Float -> editor.putFloat(key, value)
            is Set<*> -> { require(value.all { it is String }); editor.putStringSet(key, value.map { it as String }.toSet()) }
            else -> error("旧身份数据损坏")
        }
    }
    /** Called by both startup recovery and explicit removal; must not invoke ensure recursively. */
    internal suspend fun finishRemoval(context: Context, profileId: String) = withContext(NonCancellable + Dispatchers.IO) {
        val profiles = ServerProfiles.get(context)
        val profile = requireNotNull(profiles.get(profileId))
        require(profile.removalMode != null)
        AuthorizationReminders.clearForMigration(context, profileId)
        DeviceKeys(profile.wakeAlias, profile.manageAlias).removeLocalKeys()
        check(context.getSharedPreferences("roc_wol_identity_$profileId", Context.MODE_PRIVATE).edit().clear().commit()) { "服务器身份清理失败，请重试" }
        profiles.completeRemoval(profileId)
        ServerRepositories.get(context).releaseAfterRemoval(profileId)
    }
}
