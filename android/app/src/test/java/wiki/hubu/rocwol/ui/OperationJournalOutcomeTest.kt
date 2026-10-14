package wiki.hubu.rocwol.ui

import kotlinx.coroutines.CancellationException
import org.junit.Assert.assertEquals
import org.junit.Test
import wiki.hubu.rocwol.data.ApiException
import wiki.hubu.rocwol.data.NetworkException
import wiki.hubu.rocwol.data.NetworkFailureKind
import wiki.hubu.rocwol.data.journal.JournalAction
import wiki.hubu.rocwol.data.journal.JournalError
import wiki.hubu.rocwol.data.journal.JournalOutcome
import wiki.hubu.rocwol.security.AuthenticationCancelled

class OperationJournalOutcomeTest {
    @Test fun unconfirmedMutationRemainsUnknownAfterTimeoutOrCoroutineCancellation() {
        assertEquals(JournalOutcome.UNKNOWN, operationFailureOutcome(JournalAction.AUTHORIZATION_END,
            NetworkException(NetworkFailureKind.TIMEOUT), managementUncertain = true))
        assertEquals(JournalOutcome.UNKNOWN, operationFailureOutcome(JournalAction.AUTHORIZATION_START,
            CancellationException(), managementUncertain = true))
    }

    @Test fun canceledSystemAuthenticationDoesNotInheritAnOlderUnknownGrant() {
        assertEquals(JournalOutcome.CANCELLED, operationFailureOutcome(JournalAction.AUTHORIZATION_START,
            AuthenticationCancelled(), managementUncertain = true))
    }

    @Test fun unrelatedReadFailureDoesNotInheritAuthorizationUncertainty() {
        assertEquals(JournalOutcome.FAILURE, operationFailureOutcome(JournalAction.TARGET_CHECK,
            NetworkException(), managementUncertain = true))
    }

    @Test fun definitiveAuthorizationRejectionIsStillAFailure() {
        assertEquals(JournalOutcome.FAILURE, operationFailureOutcome(JournalAction.AUTHORIZATION_START,
            ApiException(403, "forbidden"), managementUncertain = false))
    }

    @Test fun controlledTargetRejectionsAreFailuresNotUnknownWakeOutcomes() {
        listOf(404 to "target_not_found", 409 to "request_target_mismatch").forEach { (status, code) ->
            assertEquals(JournalOutcome.FAILURE, operationFailureOutcome(JournalAction.WAKE_SUBMIT,
                ApiException(status, code), managementUncertain = true))
            assertEquals(code, JournalError.fromCode(code)!!.code)
        }
    }

    @Test fun managementDisabledAndProtocolUpgradeRemainControlledFailures() {
        listOf(403 to "management_disabled", 426 to "protocol_upgrade_required").forEach { (status, code) ->
            assertEquals(JournalOutcome.FAILURE, operationFailureOutcome(JournalAction.AUTHORIZATION_START,
                ApiException(status, code), managementUncertain = false))
            assertEquals(code, JournalError.fromCode(code)!!.code)
        }
    }

    @Test fun canceledReadCannotBeReportedAsSuccessOrUncertainAuthorization() {
        assertEquals(JournalOutcome.CANCELLED, operationFailureOutcome(JournalAction.TARGET_CHECK,
            CancellationException(), managementUncertain = true))
        assertEquals(JournalOutcome.CANCELLED, operationFailureOutcome(JournalAction.CONNECTION_CHECK,
            CancellationException(), managementUncertain = false))
    }
}
