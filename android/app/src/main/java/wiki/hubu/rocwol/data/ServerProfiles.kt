package wiki.hubu.rocwol.data

import android.content.Context
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import org.json.JSONArray
import org.json.JSONObject
import java.util.UUID

const val LEGACY_PROFILE_ID = "00000000-0000-0000-0000-000000000001"
internal const val LEGACY_WAKE_ALIAS = "roc_wol_wake_v1"
internal const val LEGACY_MANAGE_ALIAS = "roc_wol_manage_v1"
internal val PROFILE_ID_PATTERN = Regex("[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")

data class OperationContext(val profileId: String, val origin: String, val targetId: String? = null,
    val selectionGeneration: Long = 0, val routeIndex: Int? = null)
data class ServerOrigin(val origin: String, val routeIndex: Int, val verifiedInstanceId: String?)
data class ServerProfile(val profileId: String, val displayName: String, val instanceId: String?,
    val origins: List<ServerOrigin>, val nextRouteIndex: Int, val selectedOrigin: String?,
    val selectedTargetId: String?, val wakeAlias: String, val manageAlias: String, val removalMode: String? = null) {
    val verifiedOrigins: List<String> get() = origins.filter { instanceId != null && it.verifiedInstanceId == instanceId }.map { it.origin }
}

/** An unrecognized or corrupt store is an error, never an empty installation. */
class ServerProfiles private constructor(context: Context) {
    private val preferences = context.applicationContext.getSharedPreferences("roc_wol_servers", Context.MODE_PRIVATE)
    private val lock = Mutex()
    private val mutableProfiles = MutableStateFlow<List<ServerProfile>>(emptyList())
    private val mutableActive = MutableStateFlow<String?>(null)
    val profiles: StateFlow<List<ServerProfile>> = mutableProfiles.asStateFlow()
    val activeProfileId: StateFlow<String?> = mutableActive.asStateFlow()
    private var loaded = false
    fun get(id: String): ServerProfile? = mutableProfiles.value.firstOrNull { it.profileId == id }

    suspend fun load() = withContext(Dispatchers.IO) { lock.withLock {
        if (loaded) return@withLock
        if (!preferences.contains("schemaVersion")) {
            check(preferences.all.isEmpty()) { "服务器档案存储损坏" }
            persist(emptyList(), null)
        } else {
            check(preferences.getInt("schemaVersion", -1) == 1) { "服务器档案版本不受支持" }
            val array = JSONArray(requireNotNull(preferences.getString("profiles", null)))
            val values = (0 until array.length()).map { decode(array.getJSONObject(it)) }
            val active = preferences.getString("activeProfileId", null)
            validate(values, active)
            mutableProfiles.value = values
            mutableActive.value = active
        }
        loaded = true
    } }

    suspend fun add(name: String, origin: String): ServerProfile {
        AppMigration.ensure(app)
        val normalized = Origins.checked(origin)
        require(name.isNotBlank() && name.codePointCount(0, name.length) <= 64 && name.none { Character.isISOControl(it) })
        require(mutableProfiles.value.size < 32 && mutableProfiles.value.none { p -> p.origins.any { it.origin == normalized } })
        val info = ServerInfo.verify(normalized)
        val id = UUID.randomUUID().toString()
        val value = ServerProfile(id, name, info.instanceId, listOf(ServerOrigin(normalized, 1, info.instanceId)),
            2, normalized, null, "roc_wol_${id}_wake_v1", "roc_wol_${id}_manage_v1")
        withContext(Dispatchers.IO) { lock.withLock { persist(mutableProfiles.value + value, mutableActive.value) } }
        return value
    }
    suspend fun rename(id: String, name: String) = update(id) { it.copy(displayName = name) }
    suspend fun addOrigin(id: String, origin: String) {
        val normalized = Origins.checked(origin)
        val prior = requireNotNull(get(id))
        require(prior.removalMode == null && (prior.origins.any { it.origin == normalized } || prior.origins.size < 8))
        require(prior.nextRouteIndex < Int.MAX_VALUE)
        require(mutableProfiles.value.none { p -> p.profileId != id && p.origins.any { it.origin == normalized } })
        val info = ServerInfo.verify(normalized)
        require(prior.instanceId == null || prior.instanceId == info.instanceId) { "这条线路属于另一服务器，请另建档案并配对" }
        update(id) { current ->
            require(current.instanceId == null || current.instanceId == info.instanceId)
            val existing = current.origins.firstOrNull { it.origin == normalized }
            if (existing != null) current.copy(instanceId = info.instanceId,
                origins = current.origins.map { if (it.origin == normalized) it.copy(verifiedInstanceId = info.instanceId) else it },
                selectedOrigin = current.selectedOrigin ?: normalized)
            else current.copy(instanceId = info.instanceId, origins = current.origins + ServerOrigin(normalized, current.nextRouteIndex, info.instanceId),
                nextRouteIndex = current.nextRouteIndex + 1, selectedOrigin = current.selectedOrigin ?: normalized)
        }
    }
    suspend fun removeOrigin(id: String, origin: String) = update(id) {
        require(it.origins.size > 1 && it.selectedOrigin != origin) { "请先安全切换线路；最后一条线路只能随档案删除" }
        it.copy(origins = it.origins.filterNot { route -> route.origin == origin })
    }
    suspend fun selectActive(id: String?) = withContext(Dispatchers.IO) { lock.withLock {
        require(id == null || get(id)?.removalMode == null && get(id) != null)
        persist(mutableProfiles.value, id)
    } }
    suspend fun selectTarget(id: String, targetId: String?) = update(id) { it.copy(selectedTargetId = targetId?.let(::checkedTargetId)) }
    internal suspend fun selectOrigin(id: String, origin: String) = update(id) {
        require(origin in it.verifiedOrigins)
        it.copy(selectedOrigin = origin)
    }
    internal suspend fun update(id: String, transform: (ServerProfile) -> ServerProfile) = withContext(Dispatchers.IO) { lock.withLock {
        val prior = requireNotNull(get(id))
        require(prior.removalMode == null)
        persist(mutableProfiles.value.map { if (it.profileId == id) transform(it) else it }, mutableActive.value)
    } }
    internal suspend fun installLegacy(profile: ServerProfile) = withContext(Dispatchers.IO) { lock.withLock {
        if (get(profile.profileId) == null) persist(mutableProfiles.value + profile, mutableActive.value ?: profile.profileId)
    } }
    suspend fun remove(id: String, mode: String) = withContext(NonCancellable) {
        require(mode in listOf("identity", "profile"))
        ServerRepositories.get(app).close(id)
        withContext(Dispatchers.IO) { lock.withLock {
            val value = requireNotNull(get(id))
            require(value.removalMode == null || value.removalMode == mode)
            persist(mutableProfiles.value.map { if (it.profileId == id) it.copy(removalMode = mode) else it }, mutableActive.value)
        } }
        AppMigration.finishRemoval(app, id)
    }
    internal suspend fun completeRemoval(id: String) = withContext(Dispatchers.IO) { lock.withLock {
        val value = requireNotNull(get(id))
        require(value.removalMode != null)
        val next = if (value.removalMode == "profile") mutableProfiles.value.filterNot { it.profileId == id }
            else mutableProfiles.value.map { if (it.profileId == id) it.copy(selectedTargetId = null, removalMode = null) else it }
        persist(next, if (value.removalMode == "profile" && mutableActive.value == id) null else mutableActive.value)
    } }
    private val app = context.applicationContext
    private fun persist(values: List<ServerProfile>, active: String?) {
        validate(values, active)
        val json = JSONArray().apply { values.forEach { put(encode(it)) } }
        val editor = preferences.edit().putInt("schemaVersion", 1).putString("profiles", json.toString())
        if (active == null) editor.remove("activeProfileId") else editor.putString("activeProfileId", active)
        check(editor.commit()) { "服务器档案保存失败" }
        mutableProfiles.value = values
        mutableActive.value = active
    }
    private fun validate(values: List<ServerProfile>, active: String?) {
        require(values.size <= 32 && values.map { it.profileId }.distinct().size == values.size)
        require(active == null || values.any { it.profileId == active })
        require(values.flatMap { it.origins }.map { it.origin }.distinct().size == values.sumOf { it.origins.size })
        values.forEach { p ->
            require(PROFILE_ID_PATTERN.matches(p.profileId))
            require(p.displayName.isNotBlank() && p.displayName.codePointCount(0, p.displayName.length) <= 64 && p.displayName.none { Character.isISOControl(it) })
            require(p.instanceId == null || p.instanceId.matches(Regex("[0-9a-f]{32}")))
            require(p.origins.size in (if (p.profileId == LEGACY_PROFILE_ID) 0 else 1)..8)
            require(p.origins.map { it.routeIndex }.distinct().size == p.origins.size && p.nextRouteIndex > (p.origins.maxOfOrNull { it.routeIndex } ?: 0))
            p.origins.forEach { require(it.origin == Origins.checked(it.origin) && it.routeIndex > 0 &&
                (it.verifiedInstanceId == null || it.verifiedInstanceId == p.instanceId)) }
            require(p.selectedOrigin == null || p.selectedOrigin in p.verifiedOrigins)
            p.selectedTargetId?.let(::checkedTargetId)
            require(p.removalMode == null || p.removalMode in listOf("identity", "profile"))
            require((p.wakeAlias == "roc_wol_${p.profileId}_wake_v1" && p.manageAlias == "roc_wol_${p.profileId}_manage_v1") ||
                (p.profileId == LEGACY_PROFILE_ID && p.wakeAlias == LEGACY_WAKE_ALIAS && p.manageAlias == LEGACY_MANAGE_ALIAS))
        }
    }
    private fun encode(p: ServerProfile) = JSONObject().put("profileId", p.profileId).put("displayName", p.displayName)
        .put("instanceId", p.instanceId ?: JSONObject.NULL).put("nextRouteIndex", p.nextRouteIndex)
        .put("selectedOrigin", p.selectedOrigin ?: JSONObject.NULL).put("selectedTargetId", p.selectedTargetId ?: JSONObject.NULL)
        .put("wakeAlias", p.wakeAlias).put("manageAlias", p.manageAlias).put("removalMode", p.removalMode ?: JSONObject.NULL)
        .put("origins", JSONArray().apply { p.origins.forEach { put(JSONObject().put("origin", it.origin).put("routeIndex", it.routeIndex).put("verifiedInstanceId", it.verifiedInstanceId ?: JSONObject.NULL)) } })
    private fun decode(j: JSONObject): ServerProfile {
        fun optional(key: String): String? { require(j.has(key)); return if (j.isNull(key)) null else j.getString(key) }
        val routes = j.getJSONArray("origins")
        return ServerProfile(j.getString("profileId"), j.getString("displayName"), optional("instanceId"),
            (0 until routes.length()).map {
                val r = routes.getJSONObject(it)
                require(r.has("verifiedInstanceId"))
                ServerOrigin(r.getString("origin"), r.getInt("routeIndex"), if (r.isNull("verifiedInstanceId")) null else r.getString("verifiedInstanceId"))
            },
            j.getInt("nextRouteIndex"), optional("selectedOrigin"), optional("selectedTargetId"), j.getString("wakeAlias"), j.getString("manageAlias"), optional("removalMode"))
    }
    companion object {
        @Volatile private var instance: ServerProfiles? = null
        fun get(context: Context): ServerProfiles = instance ?: synchronized(this) { instance ?: ServerProfiles(context).also { instance = it } }
    }
}

class ServerRepositories private constructor(context: Context) {
    private val app = context.applicationContext
    private val lock = Mutex()
    private val values = mutableMapOf<String, Repository>()
    private val closing = mutableSetOf<String>()
    suspend fun get(profileId: String): Repository {
        AppMigration.ensure(app)
        return lock.withLock {
            val p = requireNotNull(ServerProfiles.get(app).get(profileId))
            check(p.removalMode == null && profileId !in closing) { "服务器身份正在移除" }
            values.getOrPut(profileId) { Repository(app, p) }
        }
    }
    suspend fun close(profileId: String) {
        val repository = lock.withLock { closing.add(profileId); values[profileId] }
        repository?.closeAndJoin()
        lock.withLock { values.remove(profileId) }
    }
    internal suspend fun releaseAfterRemoval(profileId: String) = lock.withLock { closing.remove(profileId) }
    companion object {
        @Volatile private var instance: ServerRepositories? = null
        fun get(context: Context): ServerRepositories = instance ?: synchronized(this) { instance ?: ServerRepositories(context).also { instance = it } }
    }
}
