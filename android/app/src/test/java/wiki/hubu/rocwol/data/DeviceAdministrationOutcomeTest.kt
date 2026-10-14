package wiki.hubu.rocwol.data

import kotlinx.coroutines.CancellationException
import org.junit.Assert.assertEquals
import org.junit.Test
import wiki.hubu.rocwol.data.journal.JournalOutcome

class DeviceAdministrationOutcomeTest {
    @Test fun loweredCapacityDoesNotRejectExistingDevices() {
        assertEquals(5, checkedDeviceCapacity(5, 6))
        assertEquals(100, checkedDeviceCapacity(100, 100))
    }

    @Test(expected = IllegalArgumentException::class)
    fun rejectsMissingCapacityInsteadOfAssumingFive() { checkedDeviceCapacity(0, 1) }

    @Test(expected = IllegalArgumentException::class)
    fun rejectsUnboundedDeviceLists() { checkedDeviceCapacity(100, 101) }

    @Test fun invalidResponseCannotProveSubmittedRevocationFailed() {
        assertEquals(JournalOutcome.UNKNOWN, administrationFailureOutcome(ApiException(200, "invalid_response"), true))
        assertEquals(JournalOutcome.UNKNOWN, administrationFailureOutcome(ApiException(403, "non_json_response"), true))
    }

    @Test fun onlyAnExplicitServerRejectionEstablishesFailure() {
        assertEquals(JournalOutcome.FAILURE, administrationFailureOutcome(ApiException(403, "forbidden"), true))
        assertEquals(JournalOutcome.UNKNOWN, administrationFailureOutcome(ApiException(503, "upstream_unavailable"), true))
    }

    @Test fun cancellationAfterSubmissionDoesNotClaimTheRevocationWasCanceled() {
        assertEquals(JournalOutcome.CANCELLED, administrationFailureOutcome(CancellationException(), false))
        assertEquals(JournalOutcome.UNKNOWN, administrationFailureOutcome(CancellationException(), true))
    }
}
