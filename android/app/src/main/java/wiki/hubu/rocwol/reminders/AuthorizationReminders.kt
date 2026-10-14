package wiki.hubu.rocwol.reminders

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.SharedPreferences
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import androidx.work.CoroutineWorker
import androidx.work.ExistingWorkPolicy
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import androidx.work.workDataOf
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.NonCancellable
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
import wiki.hubu.rocwol.data.AppMigration
import wiki.hubu.rocwol.data.ManagementAuthorization
import wiki.hubu.rocwol.data.Origins
import wiki.hubu.rocwol.data.ServerProfiles
import java.util.UUID
import java.util.concurrent.TimeUnit

class AuthorizationReminders private constructor(context: Context) {
    private val app = context.applicationContext
    private val preferences = app.getSharedPreferences(PREFERENCES_NAME, Context.MODE_PRIVATE)
    private val notifications = requireNotNull(app.getSystemService(NotificationManager::class.java))
    private val work by lazy { WorkManager.getInstance(app) }
    private val mutableEnabled = MutableStateFlow(preferences.getBoolean("enabled", false))
    val enabled: StateFlow<Boolean> = mutableEnabled.asStateFlow()

    init {
        ensureChannel()
        // Do not repair partially migrated snapshots or enter the barrier while holding storageLock.
        CoroutineScope(SupervisorJob() + Dispatchers.IO).launch {
            try {
                AppMigration.ensure(app)
                storageLock.withLock {
                    mutableEnabled.value = preferences.getBoolean("enabled", false)
                    if (!mutableEnabled.value) clearAllDeliveryLocked()
                    else reconcileAllLocked()
                }
            } catch (error: Exception) {
                if (error is CancellationException) throw error
                // Startup's migration barrier reports the error; leave the source available for retry.
            }
        }
    }

    suspend fun setEnabled(value: Boolean) = withContext(Dispatchers.IO) {
        AppMigration.ensure(app)
        storageLock.withLock {
            check(preferences.edit().putBoolean("enabled", value).commit())
            mutableEnabled.value = value
            if (!value) clearAllDeliveryLocked()
            else reconcileAllLocked()
        }
    }

    /** Unknown/offline or pending changes never establish that a confirmed grant was revoked. */
    suspend fun update(
        profileId: String,
        grant: ManagementAuthorization?,
        known: Boolean,
        pending: Boolean,
        replace: Boolean = false
    ) = withContext(Dispatchers.IO) {
        AppMigration.ensure(app)
        storageLock.withLock {
            if (!usableProfile(profileId)) return@withLock
            val previous = readSnapshot(preferences, profileId)
            if (grant == null) {
                if (ReminderPolicy.mayClear(known, pending)) clearLocked(profileId)
                return@withLock
            }
            // An unconfirmed refresh cannot replace a remembered grant with stale cached data.
            if (!known && previous != null) return@withLock
            val expiry = if (grant.isPermanent) null else grant.expiresAt.toEpochMilli()
            val sameGrant = previous != null && previous.origin == grant.origin &&
                previous.sessionMinutes == grant.sessionMinutes &&
                previous.allDevices == grant.allowAllDevices && previous.expiresAt == expiry
            if (sameGrant && !replace) {
                reconcile(profileId, previous)
                return@withLock
            }
            val now = System.currentTimeMillis()
            val snapshot = ReminderSnapshot(
                profileId = profileId,
                generation = UUID.randomUUID().toString(),
                origin = Origins.checked(grant.origin),
                sessionMinutes = grant.sessionMinutes,
                allDevices = grant.allowAllDevices,
                expiresAt = expiry,
                remindAt = ReminderPolicy.remindAt(now, grant.allowAllDevices, expiry)
            )
            // Persist first, so a process exit can repair this exact profile/generation.
            saveSnapshot(preferences, snapshot)
            cancelDeliveryLocked(profileId)
            reconcile(profileId, snapshot)
        }
    }

    /** Confirmed revocation or local identity removal only; never a network failure. */
    suspend fun clear(profileId: String) = withContext(Dispatchers.IO) {
        AppMigration.ensure(app)
        storageLock.withLock { clearLocked(profileId) }
    }

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

    internal suspend fun deliver(profileId: String, generation: String) = withContext(Dispatchers.IO) {
        AppMigration.ensure(app)
        storageLock.withLock {
            if (!usableProfile(profileId)) {
                cancelDeliveryLocked(profileId)
                return@withLock
            }
            val snapshot = readSnapshot(preferences, profileId) ?: return@withLock
            if (!snapshot.mayNotify(profileId, generation, preferences.getBoolean("enabled", false), System.currentTimeMillis())) {
                return@withLock
            }
            if (notificationAvailability() != NotificationAvailability.AVAILABLE) return@withLock
            val intent = Intent(app, MainActivity::class.java)
                .setData(Uri.parse(ReminderPolicy.managementUri(profileId)))
                .putExtra(OPEN_MANAGEMENT_EXTRA, true)
                .putExtra(PROFILE_ID_EXTRA, profileId)
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
            saveSnapshot(preferences, snapshot.copy(notified = true))
            try {
                notifications.notify(profileId, NOTIFICATION_ID, notification)
            } catch (_: SecurityException) {
                // Permission can be revoked between checking and posting. No retry loop.
                saveSnapshot(preferences, snapshot)
            }
        }
    }

    private fun usableProfile(profileId: String): Boolean =
        ServerProfiles.get(app).get(profileId)?.let { it.removalMode == null } == true

    private fun clearLocked(profileId: String) {
        check(preferences.edit().remove(ReminderPolicy.snapshotKey(profileId)).commit())
        cancelDeliveryLocked(profileId)
    }

    private fun cancelDeliveryLocked(profileId: String) {
        notifications.cancel(profileId, NOTIFICATION_ID)
        work.cancelAllWorkByTag(ReminderPolicy.workTag(profileId)).result.get()
    }

    /** Global off is the only ordinary operation that cancels every profile's delivery. */
    private fun clearAllDeliveryLocked() {
        work.cancelAllWorkByTag(ReminderPolicy.ALL_WORK_TAG).result.get()
        work.cancelAllWorkByTag(ReminderPolicy.LEGACY_WORK_TAG).result.get()
        snapshotProfileIds(preferences).forEach { notifications.cancel(it, NOTIFICATION_ID) }
        notifications.activeNotifications.filter { it.notification.channelId == CHANNEL_ID }
            .forEach { notifications.cancel(it.tag, it.id) }
        notifications.cancel(NOTIFICATION_ID)
    }

    private fun reconcileAllLocked() {
        // A process can exit after snapshot removal but before cancellation. Include profiles
        // without snapshots and orphaned tagged notifications so startup finishes that cleanup.
        val profileIds = snapshotProfileIds(preferences) +
            ServerProfiles.get(app).profiles.value.map { it.profileId } +
            notifications.activeNotifications
                .filter { it.notification.channelId == CHANNEL_ID }
                .mapNotNull { it.tag }
        profileIds.distinct().forEach { profileId ->
            reconcile(profileId, readSnapshot(preferences, profileId))
        }
    }

    private fun reconcile(profileId: String, snapshot: ReminderSnapshot?) {
        val now = System.currentTimeMillis()
        val enabled = preferences.getBoolean("enabled", false)
        if (!usableProfile(profileId) || snapshot == null || !enabled ||
            (snapshot.expiresAt != null && now >= snapshot.expiresAt)
        ) {
            cancelDeliveryLocked(profileId)
            return
        }
        if (!snapshot.needsWork(enabled, now) || notificationAvailability() != NotificationAvailability.AVAILABLE) {
            work.cancelAllWorkByTag(ReminderPolicy.workTag(profileId)).result.get()
            return
        }
        enqueue(work, snapshot, now)
    }

    private fun ensureChannel() {
        notifications.createNotificationChannel(
            NotificationChannel(CHANNEL_ID, "管理授权安全提醒", NotificationManager.IMPORTANCE_DEFAULT).apply {
                description = "可选的本地授权检查提醒，不会自动授权、取消授权或连接服务器"
                lockscreenVisibility = Notification.VISIBILITY_PRIVATE
            }
        )
    }

    companion object {
        const val CHANNEL_ID = "authorization_reminders"
        const val OPEN_MANAGEMENT_EXTRA = "open_management"
        const val PROFILE_ID_EXTRA = "profile_id"
        internal const val GENERATION_KEY = "generation"
        private const val PREFERENCES_NAME = "authorization_reminders"
        private const val LEGACY_MIGRATED_KEY = "legacy_snapshot_migrated"
        private const val NOTIFICATION_ID = 4101
        private val storageLock = Mutex()
        @Volatile private var instance: AuthorizationReminders? = null

        fun get(context: Context): AuthorizationReminders = instance ?: synchronized(this) {
            instance ?: AuthorizationReminders(context).also { instance = it }
        }

        /** Raw migration entry: called inside AppMigration, never calls its barrier or a business singleton. */
        suspend fun migrateLegacy(context: Context) = withContext(Dispatchers.IO + NonCancellable) {
            storageLock.withLock {
                val app = context.applicationContext
                val preferences = app.getSharedPreferences(PREFERENCES_NAME, Context.MODE_PRIVATE)
                val migrated = preferences.getBoolean(LEGACY_MIGRATED_KEY, false)
                val legacy = preferences.getString("snapshot", null)
                if (migrated && legacy == null) return@withLock
                val work = WorkManager.getInstance(app)
                if (!migrated && legacy != null) {
                    val profileId = ReminderPolicy.LEGACY_PROFILE_ID
                    // Never overwrite a scoped snapshot already written by a prior migration attempt.
                    val snapshot = readSnapshot(preferences, profileId) ?: decodeSnapshot(profileId, legacy).copy(
                        origin = Origins.checked(JSONObject(legacy).getString("origin"))
                    ).also { saveSnapshot(preferences, it) }
                    val now = System.currentTimeMillis()
                    if (snapshot.needsWork(preferences.getBoolean("enabled", false), now)) {
                        // Wait for persisted enqueue before committing completion or cancelling the old worker.
                        enqueue(work, snapshot, now)
                    }
                }
                check(preferences.edit().putBoolean(LEGACY_MIGRATED_KEY, true).commit())
                work.cancelAllWorkByTag(ReminderPolicy.LEGACY_WORK_TAG).result.get()
                requireNotNull(app.getSystemService(NotificationManager::class.java)).cancel(NOTIFICATION_ID)
                check(preferences.edit().remove("snapshot").commit())
            }
        }

        /** Raw removal recovery for AppMigration; public clear waits for the barrier instead. */
        suspend fun clearForMigration(context: Context, profileId: String) = withContext(Dispatchers.IO + NonCancellable) {
            storageLock.withLock {
                val app = context.applicationContext
                val preferences = app.getSharedPreferences(PREFERENCES_NAME, Context.MODE_PRIVATE)
                check(preferences.edit().remove(ReminderPolicy.snapshotKey(profileId)).commit())
                requireNotNull(app.getSystemService(NotificationManager::class.java)).cancel(profileId, NOTIFICATION_ID)
                WorkManager.getInstance(app).cancelAllWorkByTag(ReminderPolicy.workTag(profileId)).result.get()
            }
        }

        private fun snapshotProfileIds(preferences: SharedPreferences): List<String> =
            preferences.all.keys.filter { it.startsWith(ReminderPolicy.SNAPSHOT_PREFIX) }
                .map { it.removePrefix(ReminderPolicy.SNAPSHOT_PREFIX) }

        private fun readSnapshot(preferences: SharedPreferences, profileId: String): ReminderSnapshot? =
            preferences.getString(ReminderPolicy.snapshotKey(profileId), null)?.let { decodeSnapshot(profileId, it) }

        private fun decodeSnapshot(profileId: String, value: String): ReminderSnapshot {
            val json = JSONObject(value)
            return ReminderSnapshot(
                profileId = profileId,
                generation = json.getString("generation"),
                origin = json.getString("origin"),
                sessionMinutes = json.getInt("sessionMinutes"),
                allDevices = json.getBoolean("allDevices"),
                expiresAt = if (json.isNull("expiresAt")) null else json.getLong("expiresAt"),
                remindAt = if (json.isNull("remindAt")) null else json.getLong("remindAt"),
                notified = json.getBoolean("notified")
            )
        }

        private fun saveSnapshot(preferences: SharedPreferences, snapshot: ReminderSnapshot) {
            val json = JSONObject()
                .put("generation", snapshot.generation)
                .put("origin", snapshot.origin)
                .put("sessionMinutes", snapshot.sessionMinutes)
                .put("allDevices", snapshot.allDevices)
                .put("expiresAt", snapshot.expiresAt ?: JSONObject.NULL)
                .put("remindAt", snapshot.remindAt ?: JSONObject.NULL)
                .put("notified", snapshot.notified)
            check(preferences.edit().putString(ReminderPolicy.snapshotKey(snapshot.profileId), json.toString()).commit())
        }

        private fun enqueue(work: WorkManager, snapshot: ReminderSnapshot, now: Long) {
            val request = OneTimeWorkRequestBuilder<AuthorizationReminderWorker>()
                .setInputData(workDataOf(PROFILE_ID_EXTRA to snapshot.profileId, GENERATION_KEY to snapshot.generation))
                .addTag(ReminderPolicy.workTag(snapshot.profileId))
                .addTag(ReminderPolicy.ALL_WORK_TAG)
                .setInitialDelay((requireNotNull(snapshot.remindAt) - now).coerceAtLeast(0), TimeUnit.MILLISECONDS)
                .build()
            // KEEP preserves this generation's absolute deadline on ordinary restarts.
            work.enqueueUniqueWork(ReminderPolicy.workName(snapshot.profileId, snapshot.generation), ExistingWorkPolicy.KEEP, request)
                .result.get()
        }
    }
}

class AuthorizationReminderWorker(context: Context, parameters: WorkerParameters) : CoroutineWorker(context, parameters) {
    override suspend fun doWork(): Result {
        return try {
            // Even a legacy worker must finish the shared migration before reading its old input.
            AppMigration.ensure(applicationContext)
            val profileId = inputData.getString(AuthorizationReminders.PROFILE_ID_EXTRA) ?: return Result.success()
            val generation = inputData.getString(AuthorizationReminders.GENERATION_KEY) ?: return Result.success()
            AuthorizationReminders.get(applicationContext).deliver(profileId, generation)
            Result.success()
        } catch (error: Exception) {
            if (error is CancellationException) throw error
            Result.failure()
        }
    }
}
