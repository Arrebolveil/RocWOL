package wiki.hubu.rocwol.reminders

import java.time.Duration

internal data class ReminderSnapshot(
    val generation: String,
    val origin: String,
    val sessionMinutes: Int,
    val allDevices: Boolean,
    val expiresAt: Long?,
    val remindAt: Long?,
    val notified: Boolean = false
) {
    fun mayNotify(expectedGeneration: String, enabled: Boolean, now: Long): Boolean =
        enabled && generation == expectedGeneration && !notified &&
            remindAt != null && now >= remindAt && (expiresAt == null || now < expiresAt)
}

internal object ReminderPolicy {
    private val exposureDelay = Duration.ofMinutes(5).toMillis()
    private val permanentDelay = Duration.ofHours(24).toMillis()

    // A short grant must still have more than five minutes left to merit a deferred reminder.
    fun remindAt(now: Long, allDevices: Boolean, expiresAt: Long?): Long? = when {
        allDevices && (expiresAt == null || expiresAt - now > exposureDelay) -> now + exposureDelay
        expiresAt == null -> now + permanentDelay
        else -> null
    }
}
