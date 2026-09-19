package wiki.hubu.rocwol.reminders

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class ReminderPolicyTest {
    private fun grant(generation: String = "new", expiresAt: Long? = null, notified: Boolean = false) =
        ReminderSnapshot(generation, "https://wol.hubu.wiki", if (expiresAt == null) 0 else 10,
            true, expiresAt, remindAt = 1_000, notified = notified)

    @Test fun replacedPermanentGrantRejectsTheOldWorkerDespiteIdenticalMetadata() {
        val replacement = grant(generation = "replacement")
        assertFalse(replacement.mayNotify("old", enabled = true, now = 2_000))
        assertTrue(replacement.mayNotify("replacement", enabled = true, now = 2_000))
    }

    @Test fun delayedWorkerCannotNotifyAtOrAfterFiniteExpiry() {
        val finite = grant(expiresAt = 2_000)
        assertTrue(finite.mayNotify("new", enabled = true, now = 1_999))
        assertFalse(finite.mayNotify("new", enabled = true, now = 2_000))
        assertFalse(finite.mayNotify("new", enabled = true, now = 5_000))
    }

    @Test fun DisabledOrAlreadyDeliveredWorkCannotAlertAgain() {
        assertFalse(grant().mayNotify("new", enabled = false, now = 2_000))
        assertFalse(grant(notified = true).mayNotify("new", enabled = true, now = 2_000))
    }
}
