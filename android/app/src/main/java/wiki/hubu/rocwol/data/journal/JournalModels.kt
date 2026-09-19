package wiki.hubu.rocwol.data.journal

import wiki.hubu.rocwol.data.Origins
import wiki.hubu.rocwol.data.Repository
import java.time.Instant
import java.time.ZoneId
import java.time.format.DateTimeFormatter

enum class JournalCategory(val label: String) {
    WAKE("唤醒"), TARGET("目标状态"), AUTHORIZATION("管理授权"), IDENTITY("设备身份"),
    CONNECTION("连接诊断"), SETTINGS("设置"), DEVICE("设备管理"), REMINDER("授权提醒")
}

enum class JournalAction(val category: JournalCategory, val label: String) {
    WAKE_SUBMIT(JournalCategory.WAKE, "提交唤醒"),
    WAKE_RESULT(JournalCategory.WAKE, "唤醒结果"),
    WAKE_POLL(JournalCategory.WAKE, "查询唤醒结果"),
    WAKE_ONLINE(JournalCategory.WAKE, "确认目标在线"),
    TARGET_CHECK(JournalCategory.TARGET, "检查目标状态"),
    AUTHORIZATION_START(JournalCategory.AUTHORIZATION, "开启管理授权"),
    AUTHORIZATION_STATUS(JournalCategory.AUTHORIZATION, "检查管理授权"),
    AUTHORIZATION_END(JournalCategory.AUTHORIZATION, "结束管理授权"),
    AUTHORIZATION_BROWSER(JournalCategory.AUTHORIZATION, "打开授权浏览器"),
    PAIR(JournalCategory.IDENTITY, "配对设备"),
    IDENTITY_FORGET(JournalCategory.IDENTITY, "清除本机身份"),
    IDENTITY_LOST(JournalCategory.IDENTITY, "本机身份失效"),
    CONNECTION_CHECK(JournalCategory.CONNECTION, "连接诊断"),
    ORIGIN_CHANGE(JournalCategory.SETTINGS, "切换服务入口"),
    SETTINGS_CHANGE(JournalCategory.SETTINGS, "更改设置"),
    DEVICE_LIST(JournalCategory.DEVICE, "读取设备列表"),
    DEVICE_REVOKE(JournalCategory.DEVICE, "撤销设备"),
    REMINDER_SCHEDULE(JournalCategory.REMINDER, "安排授权提醒"),
    REMINDER_CANCEL(JournalCategory.REMINDER, "取消授权提醒"),
    REMINDER_DELIVER(JournalCategory.REMINDER, "显示授权提醒")
}

enum class JournalOutcome(val label: String) {
    STARTED("开始"), SUCCESS("成功"), FAILURE("失败"), PENDING("等待结果"), UNKNOWN("结果未知"), CANCELLED("已取消")
}
enum class JournalTrigger(val label: String) { USER("用户操作"), AUTOMATIC("自动检查") }
enum class WakeStage(val label: String) {
    SUBMITTED("已发起提交"), SENT("唤醒包已发送"), PENDING("等待结果"), FAILED("唤醒失败"),
    UNKNOWN("结果未知"), CONFIRMED_ONLINE("已确认在线"), ONLINE_UNCONFIRMED("已发包，未确认上线")
}
enum class JournalTarget(val label: String) { DESKTOP("配置的电脑") }
enum class JournalReachability(val label: String) { REACHABLE("在线"), UNREACHABLE("未探测到在线"), UNKNOWN("未知") }
enum class JournalRouter(val label: String) { AVAILABLE("可用"), UNAVAILABLE("不可用") }

/** Unknown server text is reduced to OTHER, never retained or shortened. */
enum class JournalError(val code: String) {
    NETWORK("network"), TIMEOUT("timeout"), TLS("tls"), DNS("dns"),
    UNAUTHORIZED("unauthorized"), FORBIDDEN("forbidden"), INVALID_REQUEST("invalid_request"),
    INVALID_RESPONSE("invalid_response"), NOT_FOUND("not_found"), RATE_LIMITED("rate_limited"),
    UPSTREAM_UNAVAILABLE("upstream_unavailable"), WAKE_FAILED("wake_failed"), OUTCOME_UNKNOWN("outcome_unknown"),
    REDIRECT_REFUSED("redirect_refused"), NON_JSON_RESPONSE("non_json_response"),
    IDENTITY_LOST("identity_lost"), AUTHENTICATION_CANCELLED("authentication_cancelled"),
    DEVICE_ADMINISTRATION_DENIED("device_administration_denied"), CHALLENGE_EXPIRED("challenge_consumed_or_expired"),
    MANAGEMENT_INACTIVE("management_inactive"),
    PERMISSION_DENIED("permission_denied"), STORAGE("storage"), OTHER("other");

    companion object {
        fun fromCode(code: String?): JournalError? = if (code == null) null else entries.firstOrNull { it.code == code } ?: OTHER
    }
}

/** No free text is accepted. Names, IP/MAC addresses, secrets and response content have no fields. */
data class JournalEvent(
    val action: JournalAction,
    val outcome: JournalOutcome,
    val trigger: JournalTrigger = JournalTrigger.USER,
    val origin: String? = null,
    val requestId: String? = null,
    val httpStatus: Int? = null,
    val error: JournalError? = null,
    val durationMs: Long? = null,
    val wakeStage: WakeStage? = null,
    val target: JournalTarget? = null,
    val reachability: JournalReachability? = null,
    val router: JournalRouter? = null,
    val authorizationMinutes: Int? = null,
    val allowAllDevices: Boolean? = null
) {
    val category: JournalCategory get() = action.category

    internal fun isSafe(): Boolean =
        (origin == null || origin in Origins.all) &&
            (requestId == null || REQUEST_ID.matches(requestId)) &&
            (httpStatus == null || httpStatus in 100..599) &&
            (durationMs == null || durationMs in 0..86_400_000L) &&
            (authorizationMinutes == null || authorizationMinutes in Repository.sessionMinutesOptions) &&
            (wakeStage == null || category == JournalCategory.WAKE && requestId != null) &&
            (wakeStage != WakeStage.CONFIRMED_ONLINE || action == JournalAction.WAKE_ONLINE && outcome == JournalOutcome.SUCCESS && reachability == JournalReachability.REACHABLE)

    internal val aggregates: Boolean get() = trigger == JournalTrigger.AUTOMATIC && action in AUTOMATIC_ACTIONS
    internal fun scopeKey(): String = listOf(action.name, origin.orEmpty(), requestId.orEmpty()).joinToString("|")
    internal fun fingerprint(): String = listOf(outcome.name, httpStatus, error?.name, wakeStage?.name,
        target?.name, reachability?.name, router?.name, authorizationMinutes, allowAllDevices).joinToString("|")

    companion object {
        private val REQUEST_ID = Regex("[0-9a-f]{32}")
        private val AUTOMATIC_ACTIONS = setOf(JournalAction.WAKE_POLL, JournalAction.TARGET_CHECK, JournalAction.AUTHORIZATION_STATUS)
    }
}

data class JournalRecord(
    val id: Long,
    val timestamp: Instant,
    val lastTimestamp: Instant,
    val occurrences: Int,
    val event: JournalEvent
) {
    val isFailure: Boolean get() = event.outcome == JournalOutcome.FAILURE || event.outcome == JournalOutcome.UNKNOWN
}

enum class JournalProblem(val label: String) {
    NONE("正常"), STORAGE_UNAVAILABLE("本地日志存储不可用"), QUEUE_FULL("日志队列已满，部分记录未保存"),
    INVALID_EVENT("已拒绝不符合安全约束的日志"), EXPORT_FAILED("日志导出失败")
}

data class JournalHealth(
    val loaded: Boolean = false,
    val problem: JournalProblem = JournalProblem.NONE,
    val pendingWrites: Int = 0,
    val droppedRecords: Long = 0,
    val lastConfirmedWrite: Instant? = null
)

/** This renderer is shared by entry details, clipboard and the export file. */
fun JournalRecord.safeText(): String {
    if (!event.isSafe()) return "已拒绝不符合安全约束的记录"
    val date = DateTimeFormatter.ofPattern("yyyy-MM-dd HH:mm:ss XXX").withZone(ZoneId.systemDefault())
    return buildString {
        appendLine("时间：${date.format(timestamp)}")
        appendLine("分类：${event.category.label}")
        appendLine("操作：${event.action.label} · ${event.outcome.label}")
        appendLine("来源：${event.trigger.label}")
        if (occurrences > 1) appendLine("合并次数：$occurrences；最近：${date.format(lastTimestamp)}")
        event.origin?.let { appendLine("服务入口：$it") }
        event.requestId?.let { appendLine("请求 ID：$it") }
        event.httpStatus?.let { appendLine("HTTP 状态：$it") }
        event.error?.let { appendLine("错误类别：${it.code}") }
        event.durationMs?.let { appendLine("耗时：$it 毫秒${if (occurrences > 1) "（最近一次）" else ""}") }
        event.wakeStage?.let { appendLine("唤醒阶段：${it.label}") }
        event.target?.let { appendLine("目标：${it.label}") }
        event.reachability?.let { appendLine("可达状态：${it.label}") }
        event.router?.let { appendLine("路由器：${it.label}") }
        event.authorizationMinutes?.let { appendLine("授权期限：${if (it == 0) "长期" else "$it 分钟"}") }
        event.allowAllDevices?.let { appendLine("全设备访问：${if (it) "允许" else "不允许"}") }
    }.trimEnd()
}
