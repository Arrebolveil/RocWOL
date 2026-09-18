package wiki.hubu.rocwol.ui

import android.app.Activity
import android.app.Application
import android.content.ActivityNotFoundException
import android.content.Intent
import android.net.Uri
import android.security.keystore.KeyPermanentlyInvalidatedException
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import wiki.hubu.rocwol.data.ApiException
import wiki.hubu.rocwol.data.NetworkException
import wiki.hubu.rocwol.data.Repository
import wiki.hubu.rocwol.data.Target
import wiki.hubu.rocwol.data.WakeRecord
import wiki.hubu.rocwol.security.AuthenticationCancelled
import wiki.hubu.rocwol.security.AuthenticationUnavailable
import wiki.hubu.rocwol.security.IdentityLost
import java.time.Instant

 data class ScreenState(
    val paired: Boolean = false,
    val deviceName: String = "我的手机",
    val origin: String = "",
    val busy: Boolean = false,
    val server: String = "尚未检测",
    val target: Target? = null,
    val wake: WakeRecord? = null,
    val resultText: String = "尚无唤醒请求",
    val message: String? = null,
    val waiting: Boolean = false,
    val wasOnline: Boolean = false,
    val waitUntil: Instant? = null,
    val confirmNewWake: Boolean = false,
    val confirmForget: Boolean = false,
    val managementPending: Boolean = false,
    val sessionMinutes: Int = Repository.DEFAULT_SESSION_MINUTES,
    val pendingSessionMinutes: Int = Repository.DEFAULT_SESSION_MINUTES
)

class WolViewModel(application: Application) : AndroidViewModel(application) {
    private val repository = Repository(application)
    private val mutable = MutableStateFlow(ScreenState(
        paired = repository.deviceId != null, deviceName = repository.displayName,
        origin = repository.origin, wake = repository.rememberedWake(), managementPending = repository.managementPending,
        sessionMinutes = repository.sessionMinutes, pendingSessionMinutes = repository.pendingSessionMinutes
    ).let { it.copy(resultText = it.wake?.let(::resultLabel) ?: "尚无唤醒请求") })
    val state = mutable.asStateFlow()
    private var foreground = false
    private var poll: Job? = null
    private var operation: Job? = null
    private var endOnReturn = repository.managementPending

    fun foreground(value: Boolean) {
        foreground = value
        if (!value) {
            poll?.cancel(); poll = null
            if (repository.managementPending) endOnReturn = true
            return
        }
        if (!state.value.paired || state.value.busy) return
        if (endOnReturn) endManagement()
        else if (state.value.waiting) startPolling() else refresh()
    }
    fun dismissMessage() { mutable.update { it.copy(message = null) } }
    fun dismissConfirmation() { mutable.update { it.copy(confirmNewWake = false, confirmForget = false) } }
    fun requestForget() { mutable.update { it.copy(confirmForget = true) } }
    fun forget() = runOperation {
        repository.endManagement()
        repository.forgetIdentity()
        mutable.update { ScreenState(origin = repository.origin, sessionMinutes = repository.sessionMinutes, message = "本机身份已清除；服务器上的旧设备仍需通过 CLI 撤销。") }
    }
    fun pair(activity: Activity, code: String, name: String) = runOperation {
        repository.pair(activity, code.trim().uppercase(), name.trim())
        mutable.update { it.copy(paired = true, deviceName = name.trim(), message = "配对成功", server = "可达") }
        loadTarget()
    }
    fun refresh() = runOperation { loadTarget() }
    fun selectOrigin(origin: String) {
        if (state.value.busy || origin == state.value.origin) return
        runOperation {
            repository.endManagement()
            repository.selectOrigin(origin)
            mutable.update { it.copy(origin = origin, target = null, server = "尚未检测", waiting = false, waitUntil = null, message = "线路已手动切换，不会自动重发唤醒请求。") }
            if (state.value.paired) loadTarget()
        }
    }
    fun selectSessionMinutes(minutes: Int) {
        if (state.value.busy || minutes == state.value.sessionMinutes) return
        runOperation {
            repository.selectSessionMinutes(minutes)
            mutable.update { it.copy(sessionMinutes = repository.sessionMinutes) }
        }
    }
    fun requestWake() {
        if (state.value.busy) return
        val prior = state.value.wake
        if (prior != null && prior.result in setOf("unknown", "pending") && !prior.gatewayConfirmed &&
            prior.createdAt.isAfter(Instant.now().minusSeconds(86_400))) {
            retryOriginal()
        } else if (prior?.result in setOf("unknown", "pending")) {
            mutable.update { it.copy(confirmNewWake = true) }
        } else sendNewWake()
    }
    fun sendNewWake() = runOperation {
        mutable.update { it.copy(confirmNewWake = false) }
        dispatch(repository.newWake())
    }
    fun retryOriginal() {
        val record = state.value.wake ?: return
        if (record.gatewayConfirmed || record.createdAt.isBefore(Instant.now().minusSeconds(86_400))) return
        runOperation { dispatch(record) }
    }
    fun queryOriginal() {
        val record = state.value.wake ?: return
        runOperation {
            try {
                val result = repository.queryWake(record)
                repository.rememberWake(result)
                mutable.update { it.copy(wake = result, resultText = resultLabel(result), server = "可达", target = if (result.result == "unknown") null else it.target) }
                if (result.result == "sent") beginWaiting(result)
            } catch (e: ApiException) {
                if (e.status != 404) throw e
                val unknown = record.copy(result = "unknown", gatewayConfirmed = false)
                repository.rememberWake(unknown)
                mutable.update { it.copy(wake = unknown, resultText = "网关尚无此请求记录。可明确重试原请求，仍使用相同 ID；不会自动补发。", server = "可达") }
            }
        }
    }
    fun manage(activity: Activity) = runOperation {
        repository.endManagement()
        endOnReturn = false
        val url = repository.management(activity)
        mutable.update { it.copy(managementPending = true, pendingSessionMinutes = repository.pendingSessionMinutes) }
        try {
            activity.startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(url)).addCategory(Intent.CATEGORY_BROWSABLE))
        } catch (_: ActivityNotFoundException) {
            repository.endManagement()
            mutable.update { it.copy(message = "未找到可用浏览器。管理授权已撤销，请安装浏览器后重试。") }
        } catch (_: SecurityException) {
            repository.endManagement()
            mutable.update { it.copy(message = "系统阻止打开浏览器。管理授权已撤销，请检查浏览器设置。") }
        }
    }
    fun endManagement() = runOperation {
        endOnReturn = false
        repository.endManagement()
        mutable.update { it.copy(message = "管理授权已结束，浏览器再次访问需重新认证。") }
    }
    private suspend fun dispatch(record: WakeRecord) {
        val wasOnline = state.value.target?.let { it.router == "available" && it.reachability == "reachable" } == true
        val pending = record.copy(result = "pending")
        // Commit the exact ID before any network operation. Process death must not mint a retry ID.
        repository.rememberWake(pending)
        mutable.update { it.copy(wake = pending, resultText = "请求中…", wasOnline = wasOnline, waiting = false, waitUntil = null) }
        try {
            val result = repository.sendWake(pending)
            repository.rememberWake(result)
            mutable.update { it.copy(wake = result, resultText = resultLabel(result), server = "可达", target = if (result.result == "unknown") null else it.target) }
            if (result.result == "sent" || result.result == "pending") beginWaiting(result)
        } catch (e: Exception) {
            val unknown = pending.copy(result = "unknown", gatewayConfirmed = false)
            repository.rememberWake(unknown)
            mutable.update { it.copy(wake = unknown, resultText = "请求结果未知。不会自动重发；请查询原请求，或明确重试相同 ID。") }
            throw e
        }
    }
    private fun beginWaiting(record: WakeRecord) {
        val sentAt = record.sentAt ?: record.createdAt
        val deadline = sentAt.plusSeconds(90)
        if (Instant.now().isAfter(deadline)) {
            mutable.update { it.copy(waiting = false, resultText = if (record.result != "sent") "请求结果仍未确认，请查询原请求；不会自动重发。" else if (it.wasOnline) "电脑此前已在线；唤醒包已发送" else "已发送，暂未确认上线") }
            return
        }
        mutable.update { it.copy(waiting = true, waitUntil = deadline, resultText = if (record.result != "sent") "请求处理中，正在查询原请求…" else if (it.wasOnline) "电脑当前已在线；唤醒包已发送" else "已发送，等待新的在线探测…") }
    }
    private suspend fun loadTarget() {
        val target = repository.target()
        mutable.update { old ->
            val wake = old.wake ?: target.lastWake
            val newlyReachable = old.waiting && wake?.result == "sent" && target.router == "available" && target.reachability == "reachable" &&
                target.checkedAt != null && wake.sentAt != null && target.checkedAt.isAfter(wake.sentAt)
            old.copy(target = target, server = "可达", wake = wake,
                waiting = if (newlyReachable) false else old.waiting,
                resultText = if (newlyReachable) { if (old.wasOnline) "电脑当前已在线（发送前已在线）" else "已确认在线（发送后的新探测）" }
                    else if (old.wake == null && wake != null) resultLabel(wake) else old.resultText)
        }
    }
    private fun startPolling() {
        poll?.cancel()
        if (!foreground || !state.value.waiting || state.value.busy) return
        poll = viewModelScope.launch {
            while (foreground && state.value.waiting) {
                val until = state.value.waitUntil ?: break
                if (!Instant.now().isBefore(until)) {
                    mutable.update { it.copy(waiting = false, resultText = if (it.wake?.result == "sent") "已发送，暂未确认上线" else "请求结果仍未确认，请查询原请求；不会自动重发。") }
                    break
                }
                try {
                    val record = state.value.wake
                    if (record?.result == "pending") {
                        val result = repository.queryWake(record)
                        repository.rememberWake(result)
                        mutable.update { it.copy(wake = result) }
                        if (result.result == "failed" || result.result == "unknown") {
                            mutable.update { it.copy(waiting = false, resultText = resultLabel(result)) }
                        }
                    }
                    loadTarget()
                } catch (e: CancellationException) { throw e }
                catch (e: Exception) { handleError(e, polling = true) }
                delay(3_000)
            }
        }
    }
    private fun runOperation(block: suspend () -> Unit) {
        if (state.value.busy) return
        poll?.cancel()
        mutable.update { it.copy(busy = true, message = null) }
        operation = viewModelScope.launch {
            try { block() }
            catch (e: CancellationException) { throw e }
            catch (e: Exception) { handleError(e) }
            finally {
                mutable.update { it.copy(busy = false, managementPending = repository.managementPending, pendingSessionMinutes = repository.pendingSessionMinutes) }
                if (foreground && endOnReturn && repository.managementPending) endManagement()
                else if (foreground && state.value.waiting) startPolling()
            }
        }
    }
    private suspend fun handleError(error: Exception, polling: Boolean = false) {
        if (error is IdentityLost || generateSequence<Throwable>(error) { it.cause }.any { it is KeyPermanentlyInvalidatedException }) {
            repository.forgetIdentity()
            mutable.update { it.copy(paired = false, target = null, wake = null, waiting = false, message = "设备身份已撤销、丢失或密钥永久失效，请重新配对。旧服务器记录需通过 CLI 明确撤销。") }
            return
        }
        val message = when (error) {
            is AuthenticationCancelled -> "已取消系统认证，没有生成管理票据。"
            is AuthenticationUnavailable -> "此设备无法完成逐次 CryptoObject 管理签名。需要 Android 11 及以上、安全锁屏与可用的强生物识别或锁屏凭据；不会降级管理密钥。"
            is ApiException -> when (error.code) {
                "unauthorized" -> "认证已失效，请再次操作以重新认证。配对时请检查配对码。"
                "forbidden" -> "授权被拒绝，管理必须使用通过系统认证的管理密钥。"
                "challenge_consumed_or_expired" -> "挑战已过期或使用，请重新发起。"
                "rate_limited" -> "操作过于频繁，请${error.retryAfter?.let { " $it 秒后" } ?: "稍后"}再试。"
                "upstream_unavailable" -> "服务器可达，但路由器链路不可用；无法判断电脑是否关机。"
                "non_json_response" -> "线路返回了非 API 页面。备用线路可能被 Cloudflare 边缘验证阻止，请手动选择直连。"
                "redirect_refused" -> "服务返回了重定向，已拒绝跟随；请检查当前线路。"
                "invalid_request" -> "请求未被接受，请检查配对码和设备名称。"
                "not_found" -> "未找到请求或设备。"
                else -> "服务暂时无法完成操作，请稍后重试。"
            }
            is NetworkException -> "无法连接当前线路（网络或受信 TLS 连接失败）。未自动切换线路或重发请求。"
            is IllegalArgumentException -> "输入或服务响应无效。配对码应为 32 位，设备名称最多 64 个字符。"
            else -> "操作未完成，请重试。若配对提交时断网，请在服务器检查设备记录后重新签发配对码。"
        }
        mutable.update {
            it.copy(message = (if (repository.managementPending) "管理授权尚未确认撤销，请点击“结束管理授权”重试。本次授权 ${repository.pendingSessionMinutes} 分钟；即使离线，也会在票据签发后最多 ${repository.pendingSessionMinutes + 1} 分钟内失效（含最多 1 分钟的票据兑换等待）。\n" else "") + (if (polling && it.message != null) it.message else message),
                server = if (error is NetworkException) "不可达" else if (error is ApiException) "可达" else it.server,
                target = if (error is NetworkException || (error is ApiException && error.status >= 500)) null else it.target)
        }
    }
}

fun resultLabel(record: WakeRecord): String = when (record.result) {
    "sent" -> "唤醒包已发送（不代表电脑已开机）"
    "failed" -> "路由器明确报告发送失败"
    "pending" -> "请求处理中，可查询原请求；不会再次发包"
    else -> "发送结果未知，请先查询原请求；不会自动补发"
}
