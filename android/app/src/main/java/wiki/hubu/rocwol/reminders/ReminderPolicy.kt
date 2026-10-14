package wiki.hubu.rocwol.reminders

import java.time.Duration

internal data class ReminderSnapshot(
    val profileId: String,
    val generation: String,
    val origin: String,
    val sessionMinutes: Int,
    val allDevices: Boolean,
    val expiresAt: Long?,
    val remindAt: Long?,
    val notified: Boolean = false
) {
    fun mayNotify(expectedProfileId: String, expectedGeneration: String, enabled: Boolean, now: Long): Boolean =
        profileId == expectedProfileId && enabled && generation == expectedGeneration && !notified &&
            remindAt != null && now >= remindAt && (expiresAt == null || now < expiresAt)

    fun needsWork(enabled: Boolean, now: Long): Boolean =
        enabled && !notified && remindAt != null && (expiresAt == null || now < expiresAt)
}

internal object ReminderPolicy {
    const val LEGACY_PROFILE_ID = "00000000-0000-0000-0000-000000000001"
    const val LEGACY_WORK_TAG = "authorization-reminder"
    const val ALL_WORK_TAG = "authorization-reminders"
    const val SNAPSHOT_PREFIX = "snapshot:"

    private val profileIdPattern = Regex("[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")

    private val exposureDelay = Duration.ofMinutes(5).toMillis()
    private val permanentDelay = Duration.ofHours(24).toMillis()

    fun snapshotKey(profileId: String): String = "$SNAPSHOT_PREFIX$profileId"
    fun workTag(profileId: String): String = "authorization-reminder:$profileId"
    fun workName(profileId: String, generation: String): String = "${workTag(profileId)}:$generation"
    fun managementUri(profileId: String): String = "rocwol://management/$profileId"

    /** Both intent fields must identify the same canonical, explicit management destination. */
    fun navigationProfileId(data: String?, profileId: String?, openManagement: Boolean): String? =
        profileId?.takeIf {
            openManagement && profileIdPattern.matches(it) && data == managementUri(it)
        }

    /** Unknown or pending results cannot prove that a remembered authorization has ended. */
    fun mayClear(known: Boolean, pending: Boolean): Boolean = known && !pending

    // A short grant must still have more than five minutes left to merit a deferred reminder.
    fun remindAt(now: Long, allDevices: Boolean, expiresAt: Long?): Long? = when {
        allDevices && (expiresAt == null || expiresAt - now > exposureDelay) -> now + exposureDelay
        expiresAt == null -> now + permanentDelay
        else -> null
    }
}
