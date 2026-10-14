package wiki.hubu.rocwol.data.journal

import java.time.Instant
import org.junit.Assert.*
import org.junit.Test
import wiki.hubu.rocwol.data.OperationContext

class JournalModelsTest {
    private val serverA = "11111111-1111-1111-1111-111111111111"
    private val serverB = "22222222-2222-2222-2222-222222222222"
    private val requestId = "a".repeat(32)

    private fun event(profile: String = serverA, target: String? = "private-office", route: Int? = 1) = JournalEvent(
        action = JournalAction.WAKE_POLL, outcome = JournalOutcome.PENDING, trigger = JournalTrigger.AUTOMATIC,
        context = OperationContext(profile, "https://private-service.example", targetId = target, routeIndex = route),
        requestId = requestId, wakeStage = WakeStage.PENDING
    )

    private fun record(id: Long, event: JournalEvent, legacyOrigin: String? = null) =
        JournalRecord(id, Instant.EPOCH, Instant.EPOCH, 1, event, legacyOrigin)

    @Test fun historyAndAggregationDoNotCrossServerTargetOrRoute() {
        val first = event()
        val otherServer = event(profile = serverB)
        val otherTarget = event(target = "private-laptop")
        val otherRoute = event(route = 2)
        assertNotEquals(first.scopeKey(), otherServer.scopeKey())
        assertNotEquals(first.scopeKey(), otherTarget.scopeKey())
        assertNotEquals(first.scopeKey(), otherRoute.scopeKey())
        assertNotEquals(record(1, first).historyKey, record(2, otherServer).historyKey)
        assertNotEquals(record(1, first).historyKey, record(3, otherTarget).historyKey)
        assertEquals(record(1, first).historyKey, record(4, otherRoute).historyKey)
    }

    @Test fun localStorageNeverRetainsContextOriginOrSelectionGeneration() {
        val original = event().copy(context = event().context!!.copy(selectionGeneration = 91))
        val queued = original.localOnly()
        assertEquals("", queued.context!!.origin)
        assertEquals(0L, queued.context.selectionGeneration)
        val stored = StoredEvent.from(queued, 1000)
        assertNull(stored.origin)
        assertNull(stored.target)
        assertEquals(serverA, stored.profileId)
        assertEquals("private-office", stored.targetId)
        assertEquals(1, stored.routeIndex)
        assertEquals(queued, stored.record()!!.event)
    }

    @Test fun exportAliasesAreStableAndDoNotLeakRawDimensions() {
        val first = record(1, event(), "https://legacy-private.example")
        val second = record(2, event(target = "private-laptop", route = 2))
        val third = record(3, event(profile = serverB))
        val repeated = record(4, event())
        val aliases = JournalAliases()
        val text = listOf(first, second, third, repeated).joinToString("\n\n") { it.safeText(aliases) }
        listOf(serverA, serverB, "private-office", "private-laptop", "private-service.example", "legacy-private.example").forEach {
            assertFalse("Leaked dimension: $it", text.contains(it))
        }
        assertTrue(text.contains("服务器1"))
        assertTrue(text.contains("服务器2"))
        assertTrue(text.contains("电脑1"))
        assertTrue(text.contains("电脑2"))
        assertTrue(text.contains("电脑3"))
        assertTrue(text.contains("线路1"))
        assertTrue(text.contains("线路2"))
        assertTrue(text.contains(requestId))
        assertEquals(aliases.server(first), aliases.server(repeated))
        assertEquals(aliases.target(first), aliases.target(repeated))
        assertEquals(aliases.route(first), aliases.route(repeated))
        assertEquals("服务器1", JournalAliases().server(third))
    }

    @Test fun legacyOriginsOnlyDetermineExportLocalAliases() {
        val first = record(1, event(profile = LEGACY_JOURNAL_PROFILE_ID, target = "desktop", route = null), "https://one.example")
        val second = first.copy(id = 2, legacyOrigin = "https://two.example")
        val aliases = JournalAliases()
        assertEquals("线路1", aliases.route(first))
        assertEquals("线路2", aliases.route(second))
        assertEquals("线路1", aliases.route(first))
        assertFalse(first.safeText().contains("https://one.example"))
    }

    @Test fun malformedDimensionsAreRejectedInsteadOfBecomingFreeText() {
        assertFalse(event(profile = "https://private.example").isSafe())
        assertFalse(event(target = "Office Computer").isSafe())
        assertFalse(event(target = "192.0.2.1").isSafe())
        assertFalse(event(route = 0).isSafe())
        assertFalse(event().copy(requestId = "secret").isSafe())
        assertTrue(event(target = "all").isSafe())
    }

    @Test fun onlyControlledNewServerErrorsCanBeRetained() {
        mapOf(
            "target_not_found" to JournalError.TARGET_NOT_FOUND,
            "request_target_mismatch" to JournalError.REQUEST_TARGET_MISMATCH,
            "management_disabled" to JournalError.MANAGEMENT_DISABLED,
            "protocol_upgrade_required" to JournalError.PROTOCOL_UPGRADE_REQUIRED,
            "agent_unavailable" to JournalError.AGENT_UNAVAILABLE
        ).forEach { (code, expected) -> assertEquals(expected, JournalError.fromCode(code)) }
        assertEquals(JournalError.OTHER, JournalError.fromCode("https://private.example/token-secret"))
        assertNull(JournalError.fromCode(null))
    }

    @Test fun legacyRowsRemainReadableButNewWritesRequireCapturedScope() {
        val legacy = event().copy(context = null)
        assertTrue(legacy.isSafe())
        assertFalse(legacy.isSafeToAppend())
        assertFalse(event(target = null).isSafeToAppend())
        assertFalse(event().copy(requestId = null, wakeStage = null).isSafeToAppend())
        assertTrue(event().isSafeToAppend())
        assertTrue(JournalEvent(JournalAction.AUTHORIZATION_STATUS, JournalOutcome.SUCCESS,
            context = OperationContext(serverA, "", routeIndex = 1)).isSafeToAppend())
    }

    @Test fun malformedStoredScopeCannotSilentlyBecomeAnUnknownServer() {
        val stored = StoredEvent.from(event(), 1000)
        assertNull(stored.copy(profileId = null).record())
        assertNull(stored.copy(profileId = "private-name").record())
        assertNull(stored.copy(targetId = "https://private.example").record())
        assertNull(stored.copy(routeIndex = 0).record())
        assertNotNull(stored.copy(profileId = null, targetId = null, routeIndex = null).record())
    }

    @Test fun onlineConfirmationRequiresAnActualReachableProbe() {
        val confirmed = event().copy(action = JournalAction.WAKE_ONLINE, outcome = JournalOutcome.SUCCESS,
            wakeStage = WakeStage.CONFIRMED_ONLINE, reachability = JournalReachability.REACHABLE)
        assertTrue(confirmed.isSafeToAppend())
        assertFalse(confirmed.copy(reachability = JournalReachability.UNKNOWN).isSafeToAppend())
        assertFalse(confirmed.copy(outcome = JournalOutcome.PENDING).isSafeToAppend())
        assertFalse(confirmed.copy(action = JournalAction.WAKE_RESULT).isSafeToAppend())
    }

    @Test fun aggregationIgnoresNetworkOriginButSeparatesControlledResults() {
        val original = event()
        val otherOrigin = original.copy(context = original.context!!.copy(
            origin = "https://another-private.example", selectionGeneration = 92))
        assertEquals(original.scopeKey(), otherOrigin.scopeKey())
        assertEquals(original.fingerprint(), otherOrigin.fingerprint())
        assertNotEquals(original.fingerprint(), original.copy(outcome = JournalOutcome.FAILURE,
            error = JournalError.TARGET_NOT_FOUND, wakeStage = WakeStage.FAILED).fingerprint())
    }
}
