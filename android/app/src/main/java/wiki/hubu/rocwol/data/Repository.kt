package wiki.hubu.rocwol.data

import android.app.Activity
import android.content.Context
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import org.json.JSONObject
import wiki.hubu.rocwol.security.DeviceKeys
import wiki.hubu.rocwol.security.IdentityLost
import java.security.SecureRandom
import java.time.Instant

class Repository(context: Context) {
    companion object {
        const val DEFAULT_SESSION_MINUTES = 10
        val sessionMinutesOptions = listOf(5, 10, 15, 30)
    }
    private val preferences = context.getSharedPreferences("roc_wol_identity", Context.MODE_PRIVATE)
    val keys = DeviceKeys()
    private val api = Api()
    private val authLock = Mutex()
    private var token: String? = null
    private var tokenExpiry = Instant.EPOCH
    var deviceId: String? = preferences.getString("deviceId", null)
        private set
    var displayName: String = preferences.getString("deviceName", "我的手机") ?: "我的手机"
        private set
    var origin: String = preferences.getString("origin", Origins.all.first()).takeIf { it in Origins.all } ?: Origins.all.first()
        private set
    val managementPending: Boolean get() = preferences.getBoolean("managementPending", false)
    val sessionMinutes: Int get() = preferences.getInt("sessionMinutes", DEFAULT_SESSION_MINUTES)
        .takeIf { it in sessionMinutesOptions } ?: DEFAULT_SESSION_MINUTES
    // A legacy pending marker without a duration must retain the conservative server maximum.
    val pendingSessionMinutes: Int get() = preferences.getInt("pendingSessionMinutes", sessionMinutesOptions.last())
        .takeIf { it in sessionMinutesOptions } ?: sessionMinutesOptions.last()

    suspend fun selectSessionMinutes(value: Int) = withContext(Dispatchers.IO) {
        require(value in sessionMinutesOptions)
        check(preferences.edit().putInt("sessionMinutes", value).commit())
    }

    suspend fun endManagement() {
        if (!managementPending) return
        require(authenticated("/_wol/v1/manage/end", JSONObject()).body.getBoolean("ended"))
        withContext(Dispatchers.IO) {
            check(preferences.edit().remove("managementPending").remove("pendingSessionMinutes").commit())
        }
    }

    fun rememberedWake(): WakeRecord? = try {
        val raw = preferences.getString("lastWake", null)
        if (raw == null) null else {
            val json = JSONObject(raw)
            val created = Instant.parse(json.getString("createdAt"))
            if (created.isBefore(Instant.now().minusSeconds(86_400))) null
            else WakeRecord.parse(json, created).copy(gatewayConfirmed = json.optBoolean("gatewayConfirmed", false))
        }
    } catch (_: Exception) { null }

    suspend fun rememberWake(wake: WakeRecord) = withContext(Dispatchers.IO) {
        val json = JSONObject().put("requestId", wake.requestId).put("result", wake.result)
            .put("createdAt", wake.createdAt.toString()).put("gatewayConfirmed", wake.gatewayConfirmed)
        wake.sentAt?.let { json.put("sentAt", it.toString()) }
        wake.code?.let { json.put("code", it) }
        check(preferences.edit().putString("lastWake", json.toString()).commit())
    }
    suspend fun selectOrigin(value: String) = withContext(Dispatchers.IO) {
        origin = Origins.checked(value)
        clearToken()
        check(preferences.edit().putString("origin", origin).commit())
    }
    fun clearToken() { token = null; tokenExpiry = Instant.EPOCH }
    suspend fun forgetIdentity() = withContext(Dispatchers.IO) {
        clearToken()
        deviceId = null
        keys.removeLocalKeys()
        check(preferences.edit().remove("deviceId").remove("lastWake").commit())
    }
    suspend fun pair(activity: Activity, code: String, name: String) {
        require(deviceId == null)
        require(code.matches(Regex("[A-Z2-7]{32}")))
        require(name.isNotBlank() && name.codePointCount(0, name.length) <= 64)
        keys.preparePairing(activity)
        val wake = keys.publicKey(DeviceKeys.WAKE)
        val manage = keys.publicKey(DeviceKeys.MANAGE)
        val response = api.request(origin, "/_wol/v1/pair/challenge", JSONObject()
            .put("code", code).put("deviceName", name)
            .put("wakePublicKey", DeviceKeys.encode(wake)).put("managePublicKey", DeviceKeys.encode(manage))).body
        require(response.getString("serviceId") == "roc-wol")
        val challenge = Challenge.parse(response)
        val message = listOf("ROCWOL-PAIR-V1", "roc-wol", challenge.id, challenge.nonce, DeviceKeys.hash(wake), DeviceKeys.hash(manage)).joinToString("\n")
        val wakeSignature = keys.signWake(message)
        val managementSignature = keys.signManagement(activity, message, sessionMinutes = null)
        check(Instant.now().isBefore(challenge.expiresAt)) { "Challenge expired" }
        val completed = api.request(origin, "/_wol/v1/pair/complete", JSONObject().put("challengeId", challenge.id)
            .put("wakeSignature", wakeSignature).put("manageSignature", managementSignature)).body
        val id = completed.getString("deviceId")
        require(id.matches(Regex("[0-9a-f]{32}")))
        withContext(Dispatchers.IO) {
            check(preferences.edit().putString("deviceId", id).putString("deviceName", name).commit())
            deviceId = id
            displayName = name
        }
        acceptToken(completed)
    }
    private fun acceptToken(json: JSONObject) {
        val value = json.getString("accessToken")
        require(value.matches(Regex("[A-Za-z0-9_-]{43}")))
        tokenExpiry = Instant.parse(json.getString("expiresAt"))
        token = value
    }
    private suspend fun accessToken(): String = authLock.withLock {
        token?.takeIf { Instant.now().plusSeconds(15).isBefore(tokenExpiry) }?.let { return@withLock it }
        val id = deviceId ?: throw IdentityLost()
        if (!withContext(Dispatchers.IO) { keys.hasIdentity() }) throw IdentityLost()
        try {
            val challenge = Challenge.parse(api.request(origin, "/_wol/v1/auth/challenge", JSONObject().put("deviceId", id)).body)
            val message = listOf("ROCWOL-WAKE-AUTH-V1", "roc-wol", id, challenge.id, challenge.nonce).joinToString("\n")
            val signed = keys.signWake(message)
            acceptToken(api.request(origin, "/_wol/v1/auth/complete", JSONObject().put("deviceId", id)
                .put("challengeId", challenge.id).put("signature", signed)).body)
        } catch (e: ApiException) {
            if (e.status == 401 || e.status == 403) throw IdentityLost()
            throw e
        }
        token ?: throw IdentityLost()
    }
    private suspend fun authenticated(path: String, body: JSONObject? = null): ApiReply {
        val access = accessToken()
        return try { api.request(origin, path, body, access) } catch (e: ApiException) {
            if (e.status == 401) clearToken()
            throw e
        }
    }
    suspend fun target(): Target = Target.parse(authenticated("/_wol/v1/target").body)
    suspend fun sendWake(record: WakeRecord): WakeRecord {
        val response = authenticated("/_wol/v1/wake", JSONObject().put("targetId", "desktop").put("requestId", record.requestId))
        return WakeRecord.parse(response.body, record.createdAt).also { require(it.requestId == record.requestId) }
    }
    suspend fun queryWake(record: WakeRecord): WakeRecord {
        val response = authenticated("/_wol/v1/wake/${record.requestId}")
        return WakeRecord.parse(response.body, record.createdAt).also { require(it.requestId == record.requestId) }
    }
    suspend fun management(activity: Activity): String {
        val id = deviceId ?: throw IdentityLost()
        val selectedOrigin = origin
        val selectedMinutes = sessionMinutes
        val challenge = Challenge.parse(authenticated("/_wol/v1/manage/challenge", JSONObject()
            .put("origin", selectedOrigin).put("sessionMinutes", selectedMinutes)).body)
        val message = listOf("ROCWOL-MANAGE-V1", "roc-wol", id, challenge.id, challenge.nonce, selectedOrigin).joinToString("\n")
        val signature = keys.signManagement(activity, message, sessionMinutes = selectedMinutes)
        check(Instant.now().isBefore(challenge.expiresAt)) { "Challenge expired" }
        // Persist before sending: a lost response may still have created a ticket.
        withContext(Dispatchers.IO) {
            check(preferences.edit().putBoolean("managementPending", true)
                .putInt("pendingSessionMinutes", selectedMinutes).commit())
        }
        val reply = authenticated("/_wol/v1/manage/ticket", JSONObject().put("challengeId", challenge.id).put("signature", signature)).body
        require(Instant.now().isBefore(Instant.parse(reply.getString("expiresAt"))))
        return Origins.managementUrl(reply.getString("url"), selectedOrigin)
    }
    fun newWake(): WakeRecord {
        val bytes = ByteArray(16).also { SecureRandom().nextBytes(it) }
        val id = bytes.joinToString("") { "%02x".format(it.toInt() and 255) }
        return WakeRecord(id, "pending", gatewayConfirmed = false)
    }
}
