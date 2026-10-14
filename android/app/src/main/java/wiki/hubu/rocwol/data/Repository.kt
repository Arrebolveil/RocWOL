package wiki.hubu.rocwol.data

import android.app.Activity
import android.content.Context
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import org.json.JSONObject
import wiki.hubu.rocwol.data.journal.OperationJournal
import wiki.hubu.rocwol.reminders.AuthorizationReminders
import wiki.hubu.rocwol.security.DeviceKeys
import wiki.hubu.rocwol.security.IdentityLost
import java.security.SecureRandom
import java.time.Instant

/** A repository never changes server identity. Only verified routes of this profile receive credentials. */
class Repository internal constructor(context: Context, initialProfile: ServerProfile) {
    companion object {
        const val DEFAULT_SESSION_MINUTES = 10
        val sessionMinutesOptions = listOf(5, 10, 15, 30, 0)
    }
    private val app = context.applicationContext
    val profileId = initialProfile.profileId
    private val profiles = ServerProfiles.get(app)
    val profile: ServerProfile get() = requireNotNull(profiles.get(profileId))
    private val preferences = app.getSharedPreferences("roc_wol_identity_$profileId", Context.MODE_PRIVATE)
    internal val journal = OperationJournal.get(app)
    val keys = DeviceKeys(initialProfile.wakeAlias, initialProfile.manageAlias)
    private val api = Api()
    private val authLock = Mutex()
    private data class Token(val value: String, val expiresAt: Instant)
    private val tokens = mutableMapOf<String, Token>()
    private val lifecycleLock = Mutex()
    private val operations = mutableMapOf<Job, Boolean>()
    @Volatile private var closed = false
    var deviceId: String? = preferences.getString("deviceId", null)
        private set
    var displayName = preferences.getString("deviceName", "我的手机") ?: "我的手机"
        private set
    val origin: String get() = requireNotNull(profile.selectedOrigin).also { require(it in profile.verifiedOrigins) }
    val targetSelectionInitialized: Boolean get() = preferences.getBoolean("targetSelectionInitialized", false)
    suspend fun markTargetSelectionInitialized() = local { check(preferences.edit().putBoolean("targetSelectionInitialized", true).commit()) }
    val cachedTargets: List<TargetInfo> get() = preferences.getString("cachedTargets", null)?.let {
        TargetList.parse(JSONObject(it)).targets
    } ?: emptyList()
    val managementPending: Boolean get() = preferences.getBoolean("managementPending", false)
    val sessionMinutes: Int get() = preferences.getInt("sessionMinutes", DEFAULT_SESSION_MINUTES).takeIf { it in sessionMinutesOptions } ?: DEFAULT_SESSION_MINUTES
    val autoOpenBrowser: Boolean get() = preferences.getBoolean("autoOpenBrowser", true)
    val allowAllDevices: Boolean get() = preferences.getBoolean("allowAllDevices", false)
    val warnAllDevices: Boolean get() = preferences.getBoolean("warnAllDevices", true)
    private var authorization: ManagementAuthorization? = preferences.getString("managementAuthorization", null)?.let {
        val json = JSONObject(it)
        // Migrated metadata is display-only until that origin is explicitly verified.
        ManagementAuthorization.parse(json, listOf(Origins.checked(json.getString("origin"))))
    }
    val management: ManagementAuthorization? get() = authorization?.takeIf { it.isPermanent || Instant.now().isBefore(it.expiresAt) }
    var managementKnown = false
        private set

    fun operationContext(targetId: String? = profile.selectedTargetId): OperationContext {
        val selected = origin
        return OperationContext(profileId, selected, targetId, routeIndex = profile.origins.single { it.origin == selected }.routeIndex)
    }
    suspend fun <T> withOperation(readOnly: Boolean = false, block: suspend () -> T): T {
        val job = requireNotNull(currentCoroutineContext()[Job])
        val registered = lifecycleLock.withLock {
            check(!closed && profile.removalMode == null) { "服务器身份正在移除" }
            if (operations.containsKey(job)) false else { operations[job] = readOnly; true }
        }
        try { return block() }
        finally { if (registered) withContext(NonCancellable) { lifecycleLock.withLock { operations.remove(job) } } }
    }
    suspend fun closeAndJoin() {
        val own = currentCoroutineContext()[Job]
        val pending = lifecycleLock.withLock { closed = true; operations.toMap() }
        check(own !in pending) { "不能在服务器操作内移除身份" }
        pending.filterValues { it }.keys.forEach { it.cancel() }
        pending.keys.forEach { it.join() }
        clearToken()
    }
    private fun checkedOrigin(value: String): String {
        check(!closed && profile.removalMode == null) { "服务器身份正在移除" }
        return Origins.checked(value).also { require(it in profile.verifiedOrigins) }
    }
    private suspend fun local(block: () -> Unit) = withContext(NonCancellable + Dispatchers.IO) { block() }
    suspend fun selectSessionMinutes(value: Int) { require(value in sessionMinutesOptions); local { check(preferences.edit().putInt("sessionMinutes", value).commit()) } }
    suspend fun setAutoOpenBrowser(value: Boolean) = local { check(preferences.edit().putBoolean("autoOpenBrowser", value).commit()) }
    suspend fun setWarnAllDevices(value: Boolean) = local { check(preferences.edit().putBoolean("warnAllDevices", value).commit()) }
    suspend fun setAllowAllDevices(value: Boolean) {
        if (!value && deviceId != null) { local { check(preferences.edit().putBoolean("allowAllDevices", true).commit()) }; endManagement() }
        local { check(preferences.edit().putBoolean("allowAllDevices", value).commit()) }
    }
    private suspend fun markManagementUncertain() = local {
        check(preferences.edit().putBoolean("managementPending", true).remove("pendingSessionMinutes").commit())
        managementKnown = false
    }
    private suspend fun rememberManagement(value: ManagementAuthorization?) = local {
        val editor = preferences.edit().putBoolean("managementPending", value != null).remove("pendingSessionMinutes")
        if (value == null) editor.remove("managementAuthorization") else editor.putString("managementAuthorization", value.toJson().toString())
        check(editor.commit())
        authorization = value
        managementKnown = true
    }
    suspend fun managementStatus(requestOrigin: String = origin): ManagementAuthorization? {
        managementKnown = false
        val reply = authenticated("/_wol/v2/manage/status", requestOrigin = requestOrigin)
        require(reply.status in 200..299)
        rememberManagement(ManagementAuthorization.parse(reply.body, profile.verifiedOrigins))
        return management
    }
    suspend fun endManagement(requestOrigin: String = origin) {
        checkedOrigin(requestOrigin)
        markManagementUncertain()
        val reply = authenticated("/_wol/v2/manage/end", JSONObject(), requestOrigin)
        require(reply.status in 200..299 && reply.body.get("ended") == true)
        rememberManagement(null)
        withContext(NonCancellable) { AuthorizationReminders.get(app).clear(profileId) }
    }
    fun rememberedWake(targetId: String): WakeRecord? {
        checkedTargetId(targetId)
        return preferences.getString("lastWake:$targetId", null)?.let {
            val json = JSONObject(it)
            val created = Instant.parse(json.getString("createdAt"))
            if (created.isBefore(Instant.now().minusSeconds(86_400))) null else WakeRecord.parse(json, created)
                .copy(gatewayConfirmed = json.optBoolean("gatewayConfirmed", false)).also { require(it.targetId == targetId) }
        }
    }
    suspend fun rememberWake(wake: WakeRecord) = local {
        val json = JSONObject().put("requestId", wake.requestId).put("targetId", checkedTargetId(wake.targetId)).put("result", wake.result)
            .put("createdAt", wake.createdAt.toString()).put("gatewayConfirmed", wake.gatewayConfirmed)
        wake.sentAt?.let { json.put("sentAt", it.toString()) }; wake.code?.let { json.put("code", it) }
        check(preferences.edit().putString("lastWake:${wake.targetId}", json.toString()).commit())
    }
    suspend fun selectOrigin(value: String) {
        val selected = Origins.checked(value)
        require(profile.origins.any { it.origin == selected })
        if (selected !in profile.verifiedOrigins) profiles.addOrigin(profileId, selected)
        else require(ServerInfo.verify(selected).instanceId == profile.instanceId) { "线路不属于此服务器" }
        checkedOrigin(selected)
        if (deviceId != null) endManagement(selected)
        profiles.selectOrigin(profileId, selected)
    }
    fun clearToken() { synchronized(tokens) { tokens.clear() } }
    suspend fun forgetIdentity() {
        // Reuse the crash-resumable removal transaction, including operation draining.
        // Call only after leaving withOperation, never from a registered operation.
        withContext(NonCancellable) {
            profiles.remove(profileId, "identity")
            clearToken()
            deviceId = null
            authorization = null
            managementKnown = false
        }
    }
    suspend fun pair(activity: Activity, code: String, name: String, requestOrigin: String = origin) {
        val selected = checkedOrigin(requestOrigin)
        require(deviceId == null && code.matches(Regex("[A-Z2-7]{32}")))
        require(name.isNotBlank() && name.codePointCount(0, name.length) <= 64 && name.none { Character.isISOControl(it) })
        keys.preparePairing(activity)
        val wake = keys.publicKey(keys.wakeAlias); val manage = keys.publicKey(keys.manageAlias)
        val response = api.request(selected, "/_wol/v2/pair/challenge", JSONObject().put("code", code).put("deviceName", name)
            .put("wakePublicKey", DeviceKeys.encode(wake)).put("managePublicKey", DeviceKeys.encode(manage))).body
        require(response.getString("serviceId") == "roc-wol")
        val challenge = Challenge.parse(response)
        val message = listOf("ROCWOL-PAIR-V1", "roc-wol", challenge.id, challenge.nonce, DeviceKeys.hash(wake), DeviceKeys.hash(manage)).joinToString("\n")
        val wakeSignature = keys.signWake(message)
        val managementSignature = keys.signManagement(activity, message, sessionMinutes = null, allowAllDevices = false)
        check(Instant.now().isBefore(challenge.expiresAt))
        val completed = api.request(selected, "/_wol/v2/pair/complete", JSONObject().put("challengeId", challenge.id)
            .put("wakeSignature", wakeSignature).put("manageSignature", managementSignature)).body
        val id = completed.getString("deviceId").also { require(it.matches(Regex("[0-9a-f]{32}"))) }
        local { check(preferences.edit().putString("deviceId", id).putString("deviceName", name).commit()); deviceId = id; displayName = name }
        acceptToken(selected, completed)
    }
    private fun acceptToken(selected: String, json: JSONObject) {
        val value = json.getString("accessToken").also { require(it.matches(Regex("[A-Za-z0-9_-]{43}"))) }
        synchronized(tokens) { tokens[selected] = Token(value, Instant.parse(json.getString("expiresAt"))) }
    }
    private suspend fun accessToken(selected: String): String = authLock.withLock {
        synchronized(tokens) { tokens[selected] }?.takeIf { Instant.now().plusSeconds(15).isBefore(it.expiresAt) }?.let { return@withLock it.value }
        val id = deviceId ?: throw IdentityLost()
        if (!withContext(Dispatchers.IO) { keys.hasIdentity() }) throw IdentityLost()
        try {
            val challenge = Challenge.parse(api.request(selected, "/_wol/v2/auth/challenge", JSONObject().put("deviceId", id)).body)
            val message = listOf("ROCWOL-WAKE-AUTH-V1", "roc-wol", id, challenge.id, challenge.nonce).joinToString("\n")
            acceptToken(selected, api.request(selected, "/_wol/v2/auth/complete", JSONObject().put("deviceId", id)
                .put("challengeId", challenge.id).put("signature", keys.signWake(message))).body)
        } catch (e: ApiException) { if (e.status == 401 || e.status == 403) throw IdentityLost(); throw e }
        requireNotNull(synchronized(tokens) { tokens[selected] }).value
    }
    internal suspend fun authenticated(path: String, body: JSONObject? = null, requestOrigin: String = origin): ApiReply {
        val selected = checkedOrigin(requestOrigin)
        val token = accessToken(selected)
        return try { api.request(selected, path, body, token) }
        catch (e: ApiException) { if (e.status == 401) synchronized(tokens) { tokens.remove(selected) }; throw e }
    }
    suspend fun targets(requestOrigin: String = origin): TargetList {
        val json = authenticated("/_wol/v2/targets", requestOrigin = requestOrigin).body
        val result = TargetList.parse(json)
        local { check(preferences.edit().putString("cachedTargets", json.toString()).commit()) }
        return result
    }
    suspend fun target(targetId: String, fresh: Boolean = false, requestOrigin: String = origin): Target =
        Target.parse(authenticated("/_wol/v2/targets/${checkedTargetId(targetId)}${if (fresh) "?fresh=1" else ""}", requestOrigin = requestOrigin).body)
            .also { require(it.targetId == targetId) }
    suspend fun sendWake(record: WakeRecord, requestOrigin: String = origin): WakeRecord =
        parseWake(authenticated("/_wol/v2/wake", JSONObject().put("targetId", record.targetId).put("requestId", record.requestId), requestOrigin), record)
    suspend fun queryWake(record: WakeRecord, requestOrigin: String = origin): WakeRecord =
        parseWake(authenticated("/_wol/v2/wake/${record.requestId}", requestOrigin = requestOrigin), record)
    private fun parseWake(reply: ApiReply, record: WakeRecord): WakeRecord = WakeRecord.parse(reply.body, record.createdAt).also {
        require(it.requestId == record.requestId && it.targetId == record.targetId)
        require(reply.status in 200..299 || reply.status == 404 && it.result == "failed" && it.code == "target_not_found" ||
            reply.status == 409 && it.result == "failed" && it.code == "request_target_mismatch")
    }
    suspend fun authorizeManagement(activity: Activity, requestOrigin: String = origin): ManagementAuthorization {
        val id = deviceId ?: throw IdentityLost()
        val selected = checkedOrigin(requestOrigin); val minutes = sessionMinutes; val scope = allowAllDevices
        val challenge = Challenge.parse(authenticated("/_wol/v2/manage/challenge", JSONObject().put("origin", selected)
            .put("sessionMinutes", minutes).put("allowAllDevices", scope), selected).body)
        val message = listOf("ROCWOL-MANAGE-V2", "roc-wol", id, challenge.id, challenge.nonce, selected,
            minutes.toString(), if (scope) "all-devices" else "browser-only").joinToString("\n")
        val signature = keys.signManagement(activity, message, sessionMinutes = minutes, allowAllDevices = scope)
        check(Instant.now().isBefore(challenge.expiresAt))
        markManagementUncertain()
        val reply = authenticated("/_wol/v2/manage/authorize", JSONObject().put("challengeId", challenge.id).put("signature", signature), selected)
        val granted = ManagementAuthorization.parse(reply.body, profile.verifiedOrigins)
        require(reply.status in 200..299 && granted != null && granted.origin == selected && granted.sessionMinutes == minutes && granted.allowAllDevices == scope)
        rememberManagement(granted)
        return granted
    }
    suspend fun managementBrowserUrl(requestOrigin: String = origin): String {
        val granted = managementStatus(requestOrigin) ?: throw ApiException(409, "management_inactive")
        val reply = authenticated("/_wol/v2/manage/browser", JSONObject(), granted.origin)
        require(reply.status in 200..299)
        val expiry = Instant.parse(reply.body.getString("expiresAt"))
        val authorizationExpiry = reply.body.getString("authorizationExpiresAt")
        require(Instant.parse(authorizationExpiry) == granted.expiresAt && Instant.now().isBefore(expiry) &&
            (if (granted.isPermanent) authorizationExpiry == ManagementAuthorization.PERMANENT_EXPIRY else !expiry.isAfter(granted.expiresAt)))
        return Origins.managementUrl(reply.body.getString("url"), granted.origin)
    }
    fun newWake(targetId: String): WakeRecord {
        val bytes = ByteArray(16).also { SecureRandom().nextBytes(it) }
        val id = bytes.joinToString("") { "%02x".format(it.toInt() and 255) }
        return WakeRecord(id, "pending", gatewayConfirmed = false, targetId = checkedTargetId(targetId))
    }
}
