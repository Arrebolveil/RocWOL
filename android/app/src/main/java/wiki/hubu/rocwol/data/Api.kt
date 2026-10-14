package wiki.hubu.rocwol.data

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.withContext
import org.json.JSONObject
import java.io.IOException
import java.net.URI
import java.net.ConnectException
import java.net.NoRouteToHostException
import java.net.SocketException
import java.net.SocketTimeoutException
import java.net.UnknownHostException
import java.net.URL
import java.time.Instant
import javax.net.ssl.HttpsURLConnection
import javax.net.ssl.SSLException

object Origins {
    fun checked(value: String): String {
        require(value.length in 1..512 && value.all { it.code in 33..126 } && '%' !in value)
        val uri = URI(value)
        require(uri.scheme?.lowercase() == "https" && uri.rawUserInfo == null &&
            uri.rawQuery == null && uri.rawFragment == null && uri.rawPath in listOf("", "/"))
        val host = requireNotNull(uri.host).lowercase()
        require(host.length <= 253 && !host.endsWith('.'))
        if (!host.startsWith("[")) {
            require(host.split('.').all { it.length in 1..63 && it.matches(Regex("[a-z0-9](?:[a-z0-9-]*[a-z0-9])?")) })
            if (host.all { it.isDigit() || it == '.' }) require(host.split('.').let { it.size == 4 && it.all { part -> part.toIntOrNull() in 0..255 && (part == "0" || !part.startsWith('0')) } })
        }
        require(uri.port == -1 || uri.port in 1..65535)
        val port = if (uri.port == -1 || uri.port == 443) "" else ":${uri.port}"
        val authority = requireNotNull(uri.rawAuthority)
        require(authority.equals(host, ignoreCase = true) || authority.startsWith("$host:", ignoreCase = true) &&
            authority.substring(host.length + 1).matches(Regex("[0-9]+")) && uri.port in 1..65535)
        return "https://$host$port"
    }
    fun managementUrl(value: String, origin: String): String {
        val uri = URI(value)
        require(uri.scheme == "https" && uri.rawAuthority == URI(checked(origin)).rawAuthority)
        require(uri.rawPath == "/_wol/manage" && uri.rawQuery == null && uri.userInfo == null)
        require(uri.rawFragment?.matches(Regex("t=[A-Za-z0-9_-]{43}")) == true)
        return value
    }
}

class ApiException(val status: Int, val code: String, val retryAfter: Long? = null) : IOException(code)
enum class NetworkFailureKind { DNS, TLS, TIMEOUT, CONNECTION, OTHER }

class NetworkException(val kind: NetworkFailureKind = NetworkFailureKind.OTHER) : IOException("Network request failed")

fun networkFailureKind(error: IOException): NetworkFailureKind = when (error) {
    is NetworkException -> error.kind
    is UnknownHostException -> NetworkFailureKind.DNS
    is SSLException -> NetworkFailureKind.TLS
    is SocketTimeoutException -> NetworkFailureKind.TIMEOUT
    is ConnectException, is NoRouteToHostException, is SocketException -> NetworkFailureKind.CONNECTION
    else -> NetworkFailureKind.OTHER
}
data class ApiReply(val status: Int, val body: JSONObject)

class Api {
    suspend fun request(origin: String, path: String, body: JSONObject? = null, token: String? = null): ApiReply = withContext(Dispatchers.IO) {
        require(path.startsWith("/_wol/v2/") && !path.contains('#') &&
            (!path.contains('?') || (path.matches(Regex("/_wol/v2/targets/[a-z0-9][a-z0-9_-]{0,63}\\?fresh=1")) && body == null)))
        val connection = URL(Origins.checked(origin) + path).openConnection() as HttpsURLConnection
        try {
            connection.connectTimeout = 5_000
            connection.readTimeout = 10_000
            connection.instanceFollowRedirects = false
            connection.useCaches = false
            connection.requestMethod = if (body == null) "GET" else "POST"
            connection.setRequestProperty("Accept", "application/json")
            connection.setRequestProperty("Origin", origin)
            token?.let { connection.setRequestProperty("Authorization", "Bearer $it") }
            if (body != null) {
                val bytes = body.toString().toByteArray(Charsets.UTF_8)
                require(bytes.size <= 16 * 1024)
                connection.doOutput = true
                connection.setFixedLengthStreamingMode(bytes.size)
                connection.setRequestProperty("Content-Type", "application/json; charset=utf-8")
                connection.outputStream.use { it.write(bytes) }
            }
            val status = connection.responseCode
            currentCoroutineContext().ensureActive()
            if (status in 300..399) throw ApiException(status, "redirect_refused")
            val type = connection.contentType?.substringBefore(';')?.trim()?.lowercase()
            if (type != "application/json") throw ApiException(status, "non_json_response")
            val stream = if (status in 200..299) connection.inputStream else connection.errorStream
            val bytes = stream?.use { input ->
                val output = java.io.ByteArrayOutputStream()
                val buffer = ByteArray(4096)
                while (output.size() <= 65_536) {
                    currentCoroutineContext().ensureActive()
                    val count = input.read(buffer, 0, minOf(buffer.size, 65_537 - output.size()))
                    if (count < 0) break
                    output.write(buffer, 0, count)
                }
                output.toByteArray()
            } ?: throw NetworkException()
            if (bytes.size > 65_536) throw ApiException(status, "invalid_response")
            val json = try { JSONObject(String(bytes, Charsets.UTF_8)) } catch (_: Exception) { throw ApiException(status, "invalid_response") }
            if (status !in 200..299 && !json.has("result")) {
                throw ApiException(status, json.optString("code", "invalid_response"), connection.getHeaderField("Retry-After")?.toLongOrNull())
            }
            ApiReply(status, json)
        } catch (e: ApiException) {
            throw e
        } catch (error: IOException) {
            currentCoroutineContext().ensureActive()
            throw NetworkException(networkFailureKind(error))
        } finally {
            connection.disconnect()
        }
    }
}

data class Challenge(val id: String, val nonce: String, val expiresAt: Instant) {
    companion object {
        fun parse(json: JSONObject): Challenge {
            val id = json.getString("challengeId")
            val nonce = json.getString("nonce")
            require(id.matches(Regex("[0-9a-f]{32}")) && nonce.matches(Regex("[A-Za-z0-9_-]{43}")))
            return Challenge(id, nonce, Instant.parse(json.getString("expiresAt")))
        }
    }
}

data class ManagementAuthorization(
    val expiresAt: Instant,
    val sessionMinutes: Int,
    val allowAllDevices: Boolean,
    val origin: String
) {
    val isPermanent: Boolean get() = sessionMinutes == 0
    fun toJson(): JSONObject = JSONObject().put("active", true).put("expiresAt", expiresAt.toString())
        .put("sessionMinutes", sessionMinutes).put("allowAllDevices", allowAllDevices).put("origin", origin)

    companion object {
        const val PERMANENT_EXPIRY = "0001-01-01T00:00:00Z"
        fun parse(json: JSONObject, allowedOrigins: Collection<String>): ManagementAuthorization? {
            val active = json.get("active")
            require(active is Boolean)
            if (!active) return null
            val minutes = json.get("sessionMinutes")
            val allDevices = json.get("allowAllDevices")
            val origin = json.get("origin")
            val expiresAt = json.get("expiresAt")
            require(minutes is Int && minutes in Repository.sessionMinutesOptions)
            require(allDevices is Boolean && origin is String && expiresAt is String)
            require((minutes == 0) == (expiresAt == PERMANENT_EXPIRY))
            val normalized = Origins.checked(origin)
            require(normalized in allowedOrigins)
            return ManagementAuthorization(Instant.parse(expiresAt), minutes, allDevices, normalized)
        }
    }
}

fun checkedTargetId(value: String): String = value.also { require(it.matches(Regex("[a-z0-9][a-z0-9_-]{0,63}"))) }

data class WakeRecord(val requestId: String, val result: String, val sentAt: Instant? = null, val code: String? = null, val gatewayConfirmed: Boolean = true, val createdAt: Instant = Instant.now(), val targetId: String) {
    companion object {
        fun parse(json: JSONObject, createdAt: Instant = Instant.now()): WakeRecord {
            val id = json.getString("requestId")
            val result = json.getString("result")
            require(id.matches(Regex("[0-9a-f]{32}")) && result in setOf("pending", "sent", "failed", "unknown"))
            return WakeRecord(id, result, json.optString("sentAt").takeIf { it.isNotEmpty() }?.let(Instant::parse),
                json.optString("code").takeIf { it.isNotEmpty() }, true, createdAt, checkedTargetId(json.getString("targetId")))
        }
    }
}

data class TargetInfo(val targetId: String, val name: String, val mac: String, val ip: String) {
    companion object {
        fun parse(json: JSONObject): TargetInfo {
            val name = json.getString("name")
            require(name.isNotBlank() && name.codePointCount(0, name.length) <= 64 && name.none { Character.isISOControl(it) })
            val mac = json.getString("mac")
            val ip = json.getString("ip")
            require(mac.matches(Regex("[0-9a-fA-F]{2}(:[0-9a-fA-F]{2}){5}")))
            require(ip.split('.').let { it.size == 4 && it.all { part -> part.toIntOrNull() in 0..255 } })
            return TargetInfo(checkedTargetId(json.getString("targetId")), name, mac, ip)
        }
    }
}
data class TargetList(val targets: List<TargetInfo>, val managementEnabled: Boolean) {
    companion object {
        fun parse(json: JSONObject): TargetList {
            val list = json.getJSONArray("targets")
            require(list.length() in 1..64)
            val targets = (0 until list.length()).map { TargetInfo.parse(list.getJSONObject(it)) }
            require(targets.map { it.targetId }.distinct().size == targets.size)
            val enabled = json.get("managementEnabled")
            require(enabled is Boolean)
            return TargetList(targets, enabled)
        }
    }
}
data class Target(val name: String, val mac: String, val ip: String, val router: String, val reachability: String, val checkedAt: Instant?, val lastWake: WakeRecord?, val targetId: String) {
    companion object {
        fun parse(json: JSONObject): Target {
            val info = TargetInfo.parse(json)
            val router = json.getString("router")
            val reachability = json.getString("reachability")
            require(router in setOf("available", "unavailable") && reachability in setOf("reachable", "unreachable", "unknown"))
            val wake = json.optJSONObject("lastWake")?.let { WakeRecord.parse(it) }
            require(wake == null || wake.targetId == info.targetId)
            return Target(info.name, info.mac, info.ip, router, reachability,
                if (json.isNull("checkedAt")) null else Instant.parse(json.getString("checkedAt")), wake, info.targetId)
        }
    }
}

data class ServerInfo(val instanceId: String) {
    companion object {
        suspend fun verify(origin: String): ServerInfo {
            val reply = try { Api().request(Origins.checked(origin), "/_wol/v2/info") }
                catch (e: ApiException) { if (e.status == 404 || e.status == 426) throw ApiException(e.status, "protocol_upgrade_required") else throw e }
            val json = reply.body
            if (json.optInt("apiVersion") != 2) throw ApiException(426, "protocol_upgrade_required")
            require(json.getString("serviceId") == "roc-wol")
            return ServerInfo(json.getString("instanceId").also { require(it.matches(Regex("[0-9a-f]{32}"))) })
        }
    }
}
