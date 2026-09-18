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
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.receiveAsFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import wiki.hubu.rocwol.data.ApiException
import wiki.hubu.rocwol.data.ManagementAuthorization
import wiki.hubu.rocwol.data.NetworkException
import wiki.hubu.rocwol.data.Repository
import wiki.hubu.rocwol.data.Target
import wiki.hubu.rocwol.data.WakeRecord
import wiki.hubu.rocwol.security.AuthenticationCancelled
import wiki.hubu.rocwol.security.AuthenticationUnavailable
import wiki.hubu.rocwol.security.IdentityLost
import java.time.Duration
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
    val sessionMinutes: Int = Repository.DEFAULT_SESSION_MINUTES,
    val autoOpenBrowser: Boolean = true,
    val allowAllDevices: Boolean = false,
    val warnAllDevices: Boolean = true,
    val management: ManagementAuthorization? = null,
    val managementKnown: Boolean = false,
    val managementPending: Boolean = false
)

class WolViewModel(application: Application) : AndroidViewModel(application) {
    private val repository = Repository(application)
    private val mutable = MutableStateFlow(ScreenState(
        paired = repository.deviceId != null, deviceName = repository.displayName,
        origin = repository.origin, wake = repository.rememberedWake(), managementPending = repository.managementPending,
        sessionMinutes = repository.sessionMinutes, autoOpenBrowser = repository.autoOpenBrowser,
        allowAllDevices = repository.allowAllDevices, warnAllDevices = repository.warnAllDevices,
        management = repository.management
    ).let { it.copy(resultText = it.wake?.let(::resultLabel) ?: "尚无唤醒请求") })
    val state = mutable.asStateFlow()
    private val noticeChannel = Channel<String>(Channel.CONFLATED)
    val notices: Flow<String> = noticeChannel.receiveAsFlow()
    private var foreground = false
    private var poll: Job? = null
    private var operation: Job? = null
    private var managementExpiry: Job? = null
    private var managementRefreshRequested = false
    private val mutationLock = Mutex()

    fun foreground(value: Boolean) {
        foreground = value
        if (!value) {
            poll?.cancel(); poll = null
            return
        }
        if (!state.value.paired) return
        managementRefreshRequested = true
        if (!state.value.busy) refresh()
    }
    fun dismissMessage() { mutable.update { it.copy(message = null) } }
    fun dismissConfirmation() { mutable.update { it.copy(confirmNewWake = false, confirmForget = false) } }
    fun requestForget() { mutable.update { it.copy(confirmForget = true) } }
    fun forget() = runOperation {
        repository.endManagement()
        repository.forgetIdentity()
        mutable.update { ScreenState(origin = repository.origin, sessionMinutes = repository.sessionMinutes,
            autoOpenBrowser = repository.autoOpenBrowser, allowAllDevices = repository.allowAllDevices,
            warnAllDevices = repository.warnAllDevices,
            message = "本机身份已清除；服务器上的旧设备仍需通过 CLI 撤销。") }
    }
    fun pair(activity: Activity, code: String, name: String) = runOperation {
        repository.pair(activity, code.trim().uppercase(), name.trim())
        mutable.update { it.copy(paired = true, deviceName = name.trim(), server = "可达") }
        noticeChannel.trySend("配对成功")
        loadTarget()
        loadManagement()
    }
    fun refresh() = runOperation {
        try { loadManagement() }
        catch (e: CancellationException) { throw e }
        catch (e: Exception) { handleError(e) }
        if (state.value.paired) loadTarget(fresh = true)
    }

    fun refreshManagement() {
        if (!state.value.paired) return
        runOperation { loadManagement() }
    }
    fun selectOrigin(origin: String) {
        if (state.value.busy || origin == state.value.origin) return
        runOperation {
            if (state.value.paired) repository.endManagement()
            repository.selectOrigin(origin)
            mutable.update { it.copy(origin = origin, target = null, server = "尚未检测", waiting = false, waitUntil = null) }
            noticeChannel.trySend("线路已切换，未重发唤醒请求")
            if (state.value.paired) {
                loadManagement()
                loadTarget()
            }
        }
    }
    fun selectSessionMinutes(minutes: Int) = updatePreference {
        repository.selectSessionMinutes(minutes)
    }
    fun setAutoOpenBrowser(value: Boolean) = updatePreference {
        repository.setAutoOpenBrowser(value)
    }
    fun setWarnAllDevices(value: Boolean) = updatePreference {
        repository.setWarnAllDevices(value)
    }
    fun setAllowAllDevices(value: Boolean) = updatePreference(managementChange = !value) {
        // Even an already-off preference can coexist with an active or uncertain server grant.
        if (!value && state.value.paired) mutable.update { it.copy(allowAllDevices = true) }
        repository.setAllowAllDevices(value)
        if (!value && state.value.paired) noticeChannel.trySend("已关闭全设备访问，当前授权已取消")
    }
    private fun updatePreference(managementChange: Boolean = false, block: suspend () -> Unit) {
        viewModelScope.launch {
            mutationLock.withLock {
                if (managementChange && state.value.paired) mutable.update { it.copy(message = null) }
                try { block() }
                catch (e: CancellationException) { throw e }
                catch (e: Exception) { handleError(e) }
                finally {
                    mutable.update { it.copy(sessionMinutes = repository.sessionMinutes,
                        autoOpenBrowser = repository.autoOpenBrowser, allowAllDevices = repository.allowAllDevices,
                        warnAllDevices = repository.warnAllDevices) }
                    if (managementChange) publishManagement()
                }
            }
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
    fun authorizeManagement(activity: Activity) = runOperation {
        val openBrowser = state.value.autoOpenBrowser
        val granted = repository.authorizeManagement(activity)
        publishManagement()
        noticeChannel.trySend(if (granted.isPermanent) "已永久授权管理，可手动取消" else "已授权管理 ${granted.sessionMinutes} 分钟")
        if (openBrowser) launchManagementBrowser(activity)
    }
    fun openManagement(activity: Activity) = runOperation {
        launchManagementBrowser(activity)
    }
    private suspend fun launchManagementBrowser(activity: Activity) {
        val url = repository.managementBrowserUrl()
        publishManagement()
        try {
            activity.startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(url)).addCategory(Intent.CATEGORY_BROWSABLE))
        } catch (_: ActivityNotFoundException) {
            mutable.update { it.copy(message = "未找到可用浏览器，管理授权未取消。安装浏览器后可再次打开，也可手动取消。") }
        } catch (_: SecurityException) {
            mutable.update { it.copy(message = "系统阻止打开浏览器，管理授权没有取消。检查浏览器设置后可再次打开，也可手动取消。") }
        }
    }
    fun endManagement() = runOperation {
        repository.endManagement()
        publishManagement()
        noticeChannel.trySend("管理授权已取消，唤醒功能不受影响")
    }
    private suspend fun loadManagement() {
        managementRefreshRequested = false
        try { repository.managementStatus() }
        finally { publishManagement() }
    }
    private fun publishManagement() {
        val granted = repository.management
        mutable.update { it.copy(management = granted, managementKnown = repository.managementKnown,
            managementPending = repository.managementPending) }
        managementExpiry?.cancel()
        managementExpiry = granted?.takeUnless { it.isPermanent }?.let { authorization ->
            viewModelScope.launch {
                delay(Duration.between(Instant.now(), authorization.expiresAt).toMillis().coerceAtLeast(1))
                // Reaching the cached server deadline is not confirmation of a successful server revocation.
                mutable.update { if (it.management == authorization) it.copy(management = null) else it }
                if (foreground && state.value.paired) {
                    managementRefreshRequested = true
                    if (!state.value.busy) runOperation(clearMessage = false) { loadManagement() }
                }
            }
        }
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
    private suspend fun loadTarget(fresh: Boolean = false) {
        val target = repository.target(fresh)
        currentCoroutineContext().ensureActive()
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
                // Leave a fresh manual result visible before resuming cached status polling.
                delay(3_000)
                mutationLock.withLock {
                    val until = state.value.waitUntil
                    if (until != null && !Instant.now().isBefore(until)) {
                        mutable.update { it.copy(waiting = false, resultText = if (it.wake?.result == "sent") "已发送，暂未确认上线" else "请求结果未知，请查询原请求") }
                    } else if (until != null && foreground && state.value.waiting) {
                        try {
                            val record = state.value.wake
                            if (record?.result == "pending") {
                                val result = repository.queryWake(record)
                                repository.rememberWake(result)
                                currentCoroutineContext().ensureActive()
                                mutable.update { it.copy(wake = result) }
                                if (result.result == "failed" || result.result == "unknown") {
                                    mutable.update { it.copy(waiting = false, resultText = resultLabel(result)) }
                                }
                            }
                            loadTarget()
                        } catch (e: CancellationException) { throw e }
                        catch (e: Exception) { handleError(e, polling = true) }
                    }
                }
            }
        }
    }
    private fun runOperation(clearMessage: Boolean = true, block: suspend () -> Unit) {
        if (state.value.busy) return
        val priorPoll = poll
        poll = null
        priorPoll?.cancel()
        mutable.update { it.copy(busy = true, message = if (clearMessage) null else it.message) }
        operation = viewModelScope.launch {
            try {
                priorPoll?.cancelAndJoin()
                mutationLock.withLock {
                    try { block() }
                    catch (e: CancellationException) { throw e }
                    catch (e: Exception) { handleError(e) }
                    finally { publishManagement() }
                }
            } finally {
                mutable.update { it.copy(busy = false) }
                if (foreground && state.value.paired && managementRefreshRequested) {
                    runOperation(clearMessage = false) { loadManagement() }
                } else if (foreground && state.value.waiting) startPolling()
            }
        }
    }
    private suspend fun handleError(error: Exception, polling: Boolean = false) {
        if (error is IdentityLost || generateSequence<Throwable>(error) { it.cause }.any { it is KeyPermanentlyInvalidatedException }) {
            val uncertainManagement = repository.managementPending
            repository.forgetIdentity()
            mutable.update { it.copy(paired = false, target = null, wake = null, waiting = false,
                message = "设备身份已撤销、丢失或密钥永久失效，请重新配对。旧服务器记录需通过 CLI 明确撤销。" +
                    if (uncertainManagement) "尚未确认管理授权已取消，请在服务器撤销旧设备；永久授权不会自动到期。" else "") }
            return
        }
        if (error is AuthenticationCancelled) {
            noticeChannel.trySend("已取消系统认证，已有授权不受影响")
            return
        }
        val message = when (error) {
            is AuthenticationUnavailable -> "此设备无法完成逐次 CryptoObject 管理签名。需要 Android 11 及以上、安全锁屏与可用的强生物识别或锁屏凭据；不会降级管理密钥。"
            is ApiException -> when (error.code) {
                "unauthorized" -> "认证已失效，请再次操作以重新认证。配对时请检查配对码。"
                "forbidden" -> "授权被拒绝，管理必须使用通过系统认证的管理密钥。"
                "challenge_consumed_or_expired" -> "挑战已过期或使用，请重新发起。"
                "rate_limited" -> "操作过于频繁，请${error.retryAfter?.let { " $it 秒后" } ?: "稍后"}再试。"
                "upstream_unavailable" -> "路由器不可达，电脑状态未知。"
                "non_json_response" -> "线路返回了非 API 页面。备用线路可能被 Cloudflare 边缘验证阻止，请手动选择直连。"
                "redirect_refused" -> "服务返回了重定向，已拒绝跟随；请检查当前线路。"
                "invalid_response" -> "服务响应无效，未确认本次操作的服务器结果，请刷新状态或重试。"
                "management_inactive" -> "当前没有有效的管理授权，请先进行系统认证并授权。"
                "invalid_request" -> "请求未被接受，请检查配对码和设备名称。"
                "not_found" -> "未找到请求或设备。"
                else -> "服务暂时无法完成操作，请稍后重试。"
            }
            is NetworkException -> "当前线路连接失败（网络或 TLS）。"
            is IllegalArgumentException -> "输入或服务响应无效，请检查输入并刷新状态；配对码应为 32 位，设备名称最多 64 个字符。"
            else -> "操作未完成，请刷新状态后重试。若配对提交时断网，请在服务器检查设备记录后重新签发配对码。"
        }
        mutable.update {
            it.copy(message = if (polling && it.message != null) it.message else (if (repository.managementPending && !repository.managementKnown)
                "管理授权状态尚未确认，请刷新状态或点击“取消授权”向服务器关闭；不会因网络错误或返回 App 自动取消。\n"
                else if (repository.management != null)
                    "本次操作不会自动取消已确认的管理授权；可手动取消或刷新状态。\n"
                else "") + message,
                server = if (error is NetworkException) "不可达" else if (error is ApiException) "可达" else it.server,
                target = if (error is NetworkException || (error is ApiException && error.status >= 500)) null else it.target)
        }
    }
}

fun resultLabel(record: WakeRecord): String = when (record.result) {
    "sent" -> "唤醒包已发送"
    "failed" -> "唤醒包发送失败"
    "pending" -> "请求处理中"
    else -> "发送结果未知，请查询原请求"
}
