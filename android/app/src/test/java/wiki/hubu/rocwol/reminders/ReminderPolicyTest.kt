package wiki.hubu.rocwol.reminders

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.time.Duration

class ReminderPolicyTest {
    private val profileA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
    private val profileB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

    private fun grant(
        profileId: String = profileA,
        generation: String = "new",
        expiresAt: Long? = null,
        notified: Boolean = false
    ) = ReminderSnapshot(
        profileId, generation, "https://server.example", if (expiresAt == null) 0 else 10,
        true, expiresAt, remindAt = 1_000, notified = notified
    )

    @Test fun replacedPermanentGrantRejectsTheOldWorkerDespiteIdenticalMetadata() {
        val replacement = grant(generation = "replacement")
        assertFalse(replacement.mayNotify(profileA, "old", enabled = true, now = 2_000))
        assertTrue(replacement.mayNotify(profileA, "replacement", enabled = true, now = 2_000))
    }

    @Test fun sameGenerationOnAnotherServerCannotDeliverThisSnapshot() {
        val snapshot = grant()
        assertFalse(snapshot.mayNotify(profileB, "new", enabled = true, now = 2_000))
        assertTrue(snapshot.mayNotify(profileA, "new", enabled = true, now = 2_000))
    }

    @Test fun serversWithIdenticalGrantsHaveIndependentDeliveryState() {
        val snapshotA = grant(notified = true)
        val snapshotB = grant(profileId = profileB)
        assertFalse(snapshotA.mayNotify(profileA, "new", enabled = true, now = 2_000))
        assertTrue(snapshotB.mayNotify(profileB, "new", enabled = true, now = 2_000))
        assertFalse(snapshotA.needsWork(enabled = true, now = 2_000))
        assertTrue(snapshotB.needsWork(enabled = true, now = 2_000))
    }

    @Test fun delayedWorkerCannotNotifyAtOrAfterFiniteExpiry() {
        val finite = grant(expiresAt = 2_000)
        assertTrue(finite.mayNotify(profileA, "new", enabled = true, now = 1_999))
        assertFalse(finite.mayNotify(profileA, "new", enabled = true, now = 2_000))
        assertFalse(finite.mayNotify(profileA, "new", enabled = true, now = 5_000))
        assertFalse(finite.needsWork(enabled = true, now = 2_000))
    }

    @Test fun disabledOrAlreadyDeliveredWorkCannotAlertAgain() {
        assertFalse(grant().mayNotify(profileA, "new", enabled = false, now = 2_000))
        assertFalse(grant(notified = true).mayNotify(profileA, "new", enabled = true, now = 2_000))
        assertFalse(grant().needsWork(enabled = false, now = 2_000))
    }

    @Test fun savedFutureSnapshotCanBeScheduledButCannotNotifyEarly() {
        val future = grant().copy(remindAt = 5_000)
        assertTrue(future.needsWork(enabled = true, now = 2_000))
        assertFalse(future.mayNotify(profileA, "new", enabled = true, now = 2_000))
        assertTrue(future.mayNotify(profileA, "new", enabled = true, now = 5_000))
    }

    @Test fun snapshotWithoutDeadlineNeverSchedulesOrNotifies() {
        val privateFiniteGrant = grant(expiresAt = 10_000).copy(allDevices = false, remindAt = null)
        assertFalse(privateFiniteGrant.needsWork(enabled = true, now = 2_000))
        assertFalse(privateFiniteGrant.mayNotify(profileA, "new", enabled = true, now = 2_000))
    }

    @Test fun offlineOrPendingRevocationNeverClearsTheRememberedSnapshot() {
        assertFalse(ReminderPolicy.mayClear(known = false, pending = false))
        assertFalse(ReminderPolicy.mayClear(known = false, pending = true))
        assertFalse(ReminderPolicy.mayClear(known = true, pending = true))
        assertTrue(ReminderPolicy.mayClear(known = true, pending = false))
    }

    @Test fun storageWorkAndNavigationScopesNeverAliasAcrossServers() {
        assertEquals("snapshot:$profileA", ReminderPolicy.snapshotKey(profileA))
        assertEquals("authorization-reminder:$profileA", ReminderPolicy.workTag(profileA))
        assertEquals("authorization-reminder:$profileA:new", ReminderPolicy.workName(profileA, "new"))
        assertEquals("rocwol://management/$profileA", ReminderPolicy.managementUri(profileA))
        assertNotEquals(ReminderPolicy.snapshotKey(profileA), ReminderPolicy.snapshotKey(profileB))
        assertNotEquals(ReminderPolicy.workTag(profileA), ReminderPolicy.workTag(profileB))
        assertNotEquals(ReminderPolicy.workName(profileA, "new"), ReminderPolicy.workName(profileB, "new"))
        assertNotEquals(ReminderPolicy.managementUri(profileA), ReminderPolicy.managementUri(profileB))
        assertNotEquals(ReminderPolicy.workName(profileA, "old"), ReminderPolicy.workName(profileA, "new"))
    }

    @Test fun legacyGenerationSchedulingIsStableAndLegacyCancellationTagIsDistinct() {
        val migrated = grant(profileId = ReminderPolicy.LEGACY_PROFILE_ID, generation = "persisted-generation")
        val firstEnqueue = ReminderPolicy.workName(migrated.profileId, migrated.generation)
        val repeatedEnqueue = ReminderPolicy.workName(migrated.profileId, migrated.generation)
        assertEquals(firstEnqueue, repeatedEnqueue)
        assertNotEquals(ReminderPolicy.LEGACY_WORK_TAG, ReminderPolicy.workTag(migrated.profileId))
        assertNotEquals(ReminderPolicy.LEGACY_WORK_TAG, ReminderPolicy.ALL_WORK_TAG)
        assertTrue(migrated.needsWork(enabled = true, now = 2_000))
        assertTrue(migrated.mayNotify(migrated.profileId, migrated.generation, enabled = true, now = 2_000))
    }

    @Test fun notificationNavigationRequiresMatchingExplicitProfileAndDestination() {
        val destination = ReminderPolicy.managementUri(profileA)
        assertEquals(profileA, ReminderPolicy.navigationProfileId(destination, profileA, true))
        assertEquals(ReminderPolicy.LEGACY_PROFILE_ID, ReminderPolicy.navigationProfileId(
            ReminderPolicy.managementUri(ReminderPolicy.LEGACY_PROFILE_ID), ReminderPolicy.LEGACY_PROFILE_ID, true
        ))
        assertNull(ReminderPolicy.navigationProfileId(destination, profileB, true))
        assertNull(ReminderPolicy.navigationProfileId(destination, profileA, false))
        assertNull(ReminderPolicy.navigationProfileId(destination, null, true))
        assertNull(ReminderPolicy.navigationProfileId(null, profileA, true))
    }

    @Test fun ambiguousOrNoncanonicalManagementDestinationsAreRejected() {
        val invalidDestinations = listOf(
            "https://management/$profileA",
            "rocwol://management:443/$profileA",
            "rocwol://user@management/$profileA",
            "rocwol://management/$profileA/",
            "rocwol://management/$profileA?profile_id=$profileB",
            "rocwol://management/$profileA#management",
            "rocwol://management/%61${profileA.drop(1)}"
        )
        invalidDestinations.forEach { destination ->
            assertNull(ReminderPolicy.navigationProfileId(destination, profileA, true))
        }
        listOf("", "not-a-profile", "../$profileA", profileA.uppercase()).forEach { invalidProfile ->
            assertNull(ReminderPolicy.navigationProfileId(ReminderPolicy.managementUri(invalidProfile), invalidProfile, true))
        }
    }

    @Test fun finitePrivateGrantNeedsNoDeferredReminder() {
        assertNull(ReminderPolicy.remindAt(1_000, allDevices = false, expiresAt = 1_000 + Duration.ofHours(1).toMillis()))
    }

    @Test fun broadExposureReminderRequiresMoreThanFiveMinutesRemaining() {
        val now = 1_000L
        val delay = Duration.ofMinutes(5).toMillis()
        assertNull(ReminderPolicy.remindAt(now, allDevices = true, expiresAt = now + delay))
        assertEquals(now + delay, ReminderPolicy.remindAt(now, allDevices = true, expiresAt = now + delay + 1))
        assertEquals(now + delay, ReminderPolicy.remindAt(now, allDevices = true, expiresAt = null))
        assertEquals(now + Duration.ofHours(24).toMillis(), ReminderPolicy.remindAt(now, allDevices = false, expiresAt = null))
    }
}
