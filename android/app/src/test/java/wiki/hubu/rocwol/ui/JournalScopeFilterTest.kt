package wiki.hubu.rocwol.ui

import java.time.Instant
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import wiki.hubu.rocwol.data.OperationContext
import wiki.hubu.rocwol.data.ServerProfile
import wiki.hubu.rocwol.data.journal.JournalAliases
import wiki.hubu.rocwol.data.journal.LEGACY_JOURNAL_PROFILE_ID
import wiki.hubu.rocwol.data.journal.JournalAction
import wiki.hubu.rocwol.data.journal.JournalEvent
import wiki.hubu.rocwol.data.journal.JournalOutcome
import wiki.hubu.rocwol.data.journal.JournalRecord

class JournalScopeFilterTest {
    private val serverA = "11111111-1111-1111-1111-111111111111"
    private val serverB = "22222222-2222-2222-2222-222222222222"
    private fun record(profile: String?, target: String? = "pc-a") = JournalRecord(
        1, Instant.EPOCH, Instant.EPOCH, 1,
        JournalEvent(JournalAction.TARGET_CHECK, JournalOutcome.SUCCESS,
            context = profile?.let { OperationContext(it, "", targetId = target) })
    )

    @Test fun currentServerIsDefaultAndAllServersIsExplicit() {
        assertTrue(matchesJournalScope(record(serverA), "current", "all", serverA, "pc-a"))
        assertFalse(matchesJournalScope(record(serverB), "current", "all", serverA, "pc-a"))
        assertTrue(matchesJournalScope(record(serverB), "all", "all", serverA, "pc-a"))
        assertFalse(matchesJournalScope(record(serverA), "current", "all", null, null))
        assertFalse(matchesJournalScope(record(null), "current", "all", null, null))
        assertTrue(matchesJournalScope(record(null), "unknown", "all", null, null))
    }

    @Test fun identicalTargetIdsRemainIndependentAcrossServers() {
        val filter = "target:$serverA|pc-a"
        assertTrue(matchesJournalScope(record(serverA), "all", filter, serverA, "pc-a"))
        assertFalse(matchesJournalScope(record(serverB), "all", filter, serverA, "pc-a"))
        assertFalse(matchesJournalScope(record(serverA, "pc-b"), "all", filter, serverA, "pc-a"))
        assertFalse(matchesJournalScope(record(serverB), "all", "current", serverA, "pc-a"))
    }

    @Test fun protocolTargetIdsCannotCollideWithFilterModes() {
        assertTrue(matchesJournalScope(record(serverA, "all"), "current", "target:$serverA|all", serverA, "pc-a"))
        assertFalse(matchesJournalScope(record(serverA, "pc-a"), "current", "target:$serverA|all", serverA, "pc-a"))
        assertTrue(matchesJournalScope(record(serverA, null), "all", "target:$serverA|", serverA, "pc-a"))
        assertFalse(matchesJournalScope(record(serverB, null), "all", "target:$serverA|", serverA, "pc-a"))
    }

    @Test fun removedServerFiltersNeverFallBackToAnotherActiveServer() {
        assertTrue(matchesJournalScope(record(serverA), "profile:$serverA", "all", serverB, "pc-a"))
        assertFalse(matchesJournalScope(record(serverB), "profile:$serverA", "all", serverB, "pc-a"))
        assertEquals("已移除服务器 · 服务器1", journalServerLabel(record(serverA), emptyList(), JournalAliases()))
        assertEquals("已移除服务器 · 原有服务器（历史）",
            journalServerLabel(record(LEGACY_JOURNAL_PROFILE_ID), emptyList(), JournalAliases()))
    }

    @Test fun existingProfileNamesAreOnlyResolvedForLocalDisplay() {
        val profile = ServerProfile(serverA, "本地私密服务器名称", null, emptyList(), 1,
            null, null, "unused-wake", "unused-manage")
        assertEquals(profile.displayName, journalServerLabel(record(serverA), listOf(profile), JournalAliases()))
    }

    @Test fun malformedFilterModesCannotExpandTheirScope() {
        assertFalse(matchesJournalScope(record(serverA), serverA, "all", serverA, "pc-a"))
        assertFalse(matchesJournalScope(record(serverA), "all", "target:pc-a", serverA, "pc-a"))
        assertFalse(matchesJournalScope(record(serverA), "all", "unexpected", serverA, "pc-a"))
    }
}
