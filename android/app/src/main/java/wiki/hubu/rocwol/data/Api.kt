package wiki.hubu.rocwol.data

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.json.JSONObject
import wiki.hubu.rocwol.BuildConfig
import java.io.IOException
import java.net.URI
import java.net.URL
import java.time.Instant
import javax.net.ssl.HttpsURLConnection

object Origins {
    val all = listOf(BuildConfig.DIRECT_ORIGIN, BuildConfig.FALLBACK_ORIGIN)
    fun checked(value: String): String = value.also { require(it in all) }
    fun managementUrl(value: String, origin: String): String {
        val uri = URI(value)
        require(uri.scheme == "https" && uri.rawAuthority == URI(checked(origin)).rawAuthority)
        require(uri.rawPath == "/_wol/manage" && uri.rawQuery == null && uri.userInfo == null)
        require(uri.rawFragment?.matches(Regex("t=[A-Za-z0-9_-]{43}")) == true)
        return value
    }
}

class ApiException(val status: Int, val code: String, val retryAfter: Long? = null) : IOException(code)
class NetworkException : IOException("Network request failed")
data class ApiReply(val status: Int, val body: JSONObject)

class Api {
    suspend fun request(origin: String, path: String, body: JSONObject? = null, token: String? = null): ApiReply = withContext(Dispatchers.IO) {
        require(path.startsWith("/_wol/v1/") && !path.contains('?') && !path.contains('#'))
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
            if (status in 300..399) throw ApiException(status, "redirect_refused")
            val type = connection.contentType?.substringBefore(';')?.trim()?.lowercase()
            if (type != "application/json") throw ApiException(status, "non_json_response")
            val stream = if (status in 200..299) connection.inputStream else connection.errorStream
            val bytes = stream?.use { input ->
                val output = java.io.ByteArrayOutputStream()
                val buffer = ByteArray(4096)
                while (output.size() <= 65_536) {
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
        } catch (_: IOException) {
            throw NetworkException()
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
    fun toJson(): JSONObject = JSONObject().put("active", true).put("expiresAt", expiresAt.toString())
        .put("sessionMinutes", sessionMinutes).put("allowAllDevices", allowAllDevices).put("origin", origin)

    companion object {
        fun parse(json: JSONObject): ManagementAuthorization? {
            val active = json.get("active")
            require(active is Boolean)
            if (!active) return null
            val minutes = json.get("sessionMinutes")
            val allDevices = json.get("allowAllDevices")
            val origin = json.get("origin")
            val expiresAt = json.get("expiresAt")
            require(minutes is Int && minutes in Repository.sessionMinutesOptions)
            require(allDevices is Boolean && origin is String && expiresAt is String)
            return ManagementAuthorization(Instant.parse(expiresAt), minutes, allDevices, Origins.checked(origin))
        }
    }
}

data class WakeRecord(val requestId: String, val result: String, val sentAt: Instant? = null, val code: String? = null, val gatewayConfirmed: Boolean = true, val createdAt: Instant = Instant.now()) {
    companion object {
        fun parse(json: JSONObject, createdAt: Instant = Instant.now()): WakeRecord {
            val id = json.getString("requestId")
            val result = json.getString("result")
            require(id.matches(Regex("[0-9a-f]{32}")) && result in setOf("pending", "sent", "failed", "unknown"))
            return WakeRecord(id, result, json.optString("sentAt").takeIf { it.isNotEmpty() }?.let(Instant::parse), json.optString("code").takeIf { it.isNotEmpty() }, true, createdAt)
        }
    }
}

data class Target(val name: String, val mac: String, val ip: String, val router: String, val reachability: String, val checkedAt: Instant?, val lastWake: WakeRecord?) {
    companion object {
        fun parse(json: JSONObject): Target {
            require(json.getString("targetId") == "desktop")
            val router = json.getString("router")
            val reachability = json.getString("reachability")
            require(router in setOf("available", "unavailable") && reachability in setOf("reachable", "unreachable", "unknown"))
            return Target(json.getString("name"), json.getString("mac"), json.getString("ip"), router, reachability,
                if (json.isNull("checkedAt")) null else Instant.parse(json.getString("checkedAt")),
                json.optJSONObject("lastWake")?.let { WakeRecord.parse(it) })
        }
    }
}
