package wiki.hubu.rocwol.data

import android.app.Activity
import kotlinx.coroutines.CancellationException
import org.json.JSONObject
import wiki.hubu.rocwol.data.journal.JournalAction
import wiki.hubu.rocwol.data.journal.JournalError
import wiki.hubu.rocwol.data.journal.JournalEvent
import wiki.hubu.rocwol.data.journal.JournalOutcome
import wiki.hubu.rocwol.security.AuthenticationCancelled
import wiki.hubu.rocwol.security.AuthenticationUnavailable
import wiki.hubu.rocwol.security.IdentityLost
import java.time.Instant

private val administrationDeviceId = Regex("[0-9a-f]{32}")

data class DeviceAdministrationAccess(val deviceId: String, val deviceAdministration: Boolean) {
    companion object {
        fun parse(json: JSONObject): DeviceAdministrationAccess {
            val id = json.getString("deviceId")
            val allowed = json.get("deviceAdministration")
            require(administrationDeviceId.matches(id) && allowed is Boolean)
            return DeviceAdministrationAccess(id, allowed)
        }
    }
}

data class AdministeredDevice(
    val deviceId: String,
    val deviceName: String,
    val createdAt: Instant,
    val current: Boolean,
    val deviceAdministration: Boolean
) {
    companion object {
        fun parse(json: JSONObject): AdministeredDevice {
            val id = json.getString("deviceId")
            val name = json.getString("deviceName")
            val current = json.get("current")
            val allowed = json.get("deviceAdministration")
            require(administrationDeviceId.matches(id) && name.isNotBlank() && name.codePointCount(0, name.length) <= 64)
            require(current is Boolean && allowed is Boolean)
            return AdministeredDevice(id, name, Instant.parse(json.getString("createdAt")), current, allowed)
        }
    }
}

data class DeviceRevocation(val deviceId: String, val revoked: Boolean, val current: Boolean) {
    companion object {
        fun parse(json: JSONObject): DeviceRevocation {
            val id = json.getString("deviceId")
            val revoked = json.get("revoked")
            val current = json.get("current")
            require(administrationDeviceId.matches(id) && revoked == true && current is Boolean)
            return DeviceRevocation(id, true, current)
        }
    }
}

class DeviceAdministration(private val repository: Repository) {
    suspend fun access(): DeviceAdministrationAccess {
        val id = repository.deviceId ?: throw IdentityLost()
        val reply = repository.authenticated("/_wol/v1/devices/access")
        require(reply.status in 200..299)
        return DeviceAdministrationAccess.parse(reply.body).also { require(it.deviceId == id) }
    }

    suspend fun list(activity: Activity): List<AdministeredDevice> {
        val origin = repository.origin
        try {
            val id = repository.deviceId ?: throw IdentityLost()
            val proof = authorize(activity, id, origin, "list", "", "查看已配对设备")
            val reply = repository.authenticated("/_wol/v1/devices/list", proof, requestOrigin = origin)
            require(reply.status in 200..299)
            val rows = reply.body.getJSONArray("devices")
            require(rows.length() in 1..5)
            val devices = List(rows.length()) { AdministeredDevice.parse(rows.getJSONObject(it)) }
            require(devices.map { it.deviceId }.distinct().size == devices.size)
            require(devices.count { it.current } == 1 && devices.all { it.current == (it.deviceId == id) })
            repository.journal.record(JournalEvent(JournalAction.DEVICE_LIST, JournalOutcome.SUCCESS, origin = origin, httpStatus = reply.status))
            return devices
        } catch (failure: Exception) {
            recordFailure(JournalAction.DEVICE_LIST, origin, failure)
            throw failure
        }
    }

    suspend fun revoke(activity: Activity, target: AdministeredDevice): DeviceRevocation {
        val origin = repository.origin
        var submitted = false
        try {
            val id = repository.deviceId ?: throw IdentityLost()
            require(administrationDeviceId.matches(target.deviceId) && target.current == (target.deviceId == id))
            val proof = authorize(activity, id, origin, "revoke", target.deviceId,
                "撤销「${target.deviceName}」${if (target.current) "（当前手机）" else ""}")
                .put("targetDeviceId", target.deviceId)
            submitted = true
            val reply = repository.authenticated("/_wol/v1/devices/revoke", proof, requestOrigin = origin)
            require(reply.status in 200..299)
            val result = DeviceRevocation.parse(reply.body)
            require(result.deviceId == target.deviceId && result.current == target.current)
            repository.journal.record(JournalEvent(JournalAction.DEVICE_REVOKE, JournalOutcome.SUCCESS, origin = origin, httpStatus = reply.status))
            return result
        } catch (failure: Exception) {
            recordFailure(JournalAction.DEVICE_REVOKE, origin, failure, submitted)
            throw failure
        }
    }

    private fun recordFailure(action: JournalAction, origin: String, failure: Exception, submitted: Boolean = false) {
        val error = when (failure) {
            is ApiException -> JournalError.fromCode(failure.code)
            is NetworkException -> when (failure.kind) {
                NetworkFailureKind.DNS -> JournalError.DNS
                NetworkFailureKind.TLS -> JournalError.TLS
                NetworkFailureKind.TIMEOUT -> JournalError.TIMEOUT
                NetworkFailureKind.CONNECTION, NetworkFailureKind.OTHER -> JournalError.NETWORK
            }
            is AuthenticationCancelled, is CancellationException -> JournalError.AUTHENTICATION_CANCELLED
            is AuthenticationUnavailable -> JournalError.PERMISSION_DENIED
            is IdentityLost -> JournalError.IDENTITY_LOST
            else -> JournalError.INVALID_RESPONSE
        }
        val outcome = administrationFailureOutcome(failure, submitted)
        repository.journal.record(JournalEvent(action, outcome, origin = origin,
            httpStatus = (failure as? ApiException)?.status, error = error))
    }

    private suspend fun authorize(activity: Activity, id: String, origin: String, action: String, target: String, purpose: String): JSONObject {
        val reply = repository.authenticated("/_wol/v1/devices/challenge", JSONObject()
            .put("origin", origin).put("action", action).put("targetDeviceId", target), requestOrigin = origin)
        require(reply.status in 200..299)
        val challenge = Challenge.parse(reply.body)
        val message = listOf("ROCWOL-DEVICE-ADMIN-V1", "roc-wol", id, challenge.id, challenge.nonce,
            origin, action, target).joinToString("\n")
        val signature = repository.keys.signManagement(activity, message, sessionMinutes = null, allowAllDevices = false, purpose = purpose)
        if (!Instant.now().isBefore(challenge.expiresAt)) throw ApiException(409, "challenge_consumed_or_expired")
        if (repository.deviceId != id) throw IdentityLost()
        return JSONObject().put("challengeId", challenge.id).put("signature", signature)
    }
}

internal fun administrationFailureOutcome(failure: Exception, submitted: Boolean): JournalOutcome {
    val rejected = failure is ApiException && failure.status in 400..499 && when (failure.code) {
        "unauthorized", "forbidden", "device_administration_denied", "challenge_consumed_or_expired",
        "invalid_request", "not_found", "rate_limited" -> true
        else -> false
    }
    return when {
        submitted && !rejected -> JournalOutcome.UNKNOWN
        failure is AuthenticationCancelled || failure is CancellationException -> JournalOutcome.CANCELLED
        else -> JournalOutcome.FAILURE
    }
}
