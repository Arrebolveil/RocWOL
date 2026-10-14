package wiki.hubu.rocwol.ui

import android.os.SystemClock
import androidx.activity.compose.BackHandler
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.LiveRegionMode
import androidx.compose.ui.semantics.liveRegion
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.isActive
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.launch
import wiki.hubu.rocwol.data.ConnectionDiagnostics
import wiki.hubu.rocwol.data.DiagnosticDetail
import wiki.hubu.rocwol.data.DiagnosticOutcome
import wiki.hubu.rocwol.data.DiagnosticReport
import wiki.hubu.rocwol.data.DiagnosticStage
import wiki.hubu.rocwol.data.DiagnosticStageResult
import wiki.hubu.rocwol.data.Target
import wiki.hubu.rocwol.data.Repository
import wiki.hubu.rocwol.data.OperationContext
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.withContext
import java.time.ZoneId
import java.time.format.DateTimeFormatter

@Composable
fun DiagnosticsScreen(
    repository: Repository,
    operationContext: OperationContext,
    onResult: (DiagnosticReport) -> Unit,
    onBack: () -> Unit
) {
    key(operationContext.profileId, operationContext.targetId, operationContext.origin) {
        DiagnosticsContent(repository, operationContext, onResult, onBack)
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun DiagnosticsContent(
    repository: Repository,
    operationContext: OperationContext,
    onResult: (DiagnosticReport) -> Unit,
    onBack: () -> Unit
) {
    val context = LocalContext.current
    val origin = operationContext.origin
    val routeIndex = requireNotNull(operationContext.routeIndex)
    val loadTarget: suspend () -> Target = {
        repository.withOperation(readOnly = true) {
            repository.target(requireNotNull(operationContext.targetId), requestOrigin = origin)
        }
    }
    val diagnostics = remember(context) { ConnectionDiagnostics.get(context) }
    val scope = rememberCoroutineScope()
    val currentOnResult by rememberUpdatedState(onResult)
    var report by remember { mutableStateOf(DiagnosticReport.pending(routeIndex)) }
    var activeJob by remember { mutableStateOf<Job?>(null) }
    var running by remember { mutableStateOf(false) }
    var generation by remember { mutableIntStateOf(0) }
    var started by remember { mutableStateOf(0L) }
    var finishing by remember { mutableStateOf(false) }
    DisposableEffect(Unit) {
        onDispose {
            generation++
            activeJob?.cancel()
            CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate).launch {
                diagnostics.cancelAndJoin()
            }
        }
    }
    fun cancel(publish: Boolean, leave: Boolean = false) {
        if (finishing) return
        generation++
        finishing = true
        scope.launch {
            withContext(NonCancellable) {
                activeJob?.cancelAndJoin()
                diagnostics.cancelAndJoin()
            }
            activeJob = null
            running = false
            finishing = false
            report = report.canceled(SystemClock.elapsedRealtime() - started)
            if (publish) currentOnResult(report)
            if (leave) onBack()
        }
    }
    fun leave() { cancel(publish = false, leave = true) }
    BackHandler { leave() }
    Scaffold(
        topBar = {
            TopAppBar(title = { Text("连接诊断 · ${repository.profile.displayName}") }, navigationIcon = {
                IconButton(onClick = { leave() }) {
                    Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "返回")
                }
            })
        }
    ) { padding ->
        Box(Modifier.fillMaxSize().padding(padding), contentAlignment = Alignment.TopCenter) {
            LazyColumn(
                modifier = Modifier.widthIn(max = 840.dp).fillMaxSize(),
                contentPadding = PaddingValues(20.dp),
                verticalArrangement = Arrangement.spacedBy(16.dp)
            ) {
                item {
                    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                        Text("本机诊断 · 线路 ${report.routeIndex}", style = MaterialTheme.typography.titleLarge)
                        Text("仅按需进行系统 DNS 解析与一次已认证的缓存目标查询；可能需要刷新登录令牌。不会发送唤醒包、绕过 VPN、切换线路或强制刷新目标探测。", style = MaterialTheme.typography.bodyMedium)
                        Text("HTTPS 是否收到响应由该查询判断，不单独探测；API 耗时包含必要的认证。路由器和电脑状态来自服务器缓存，不是手机直连探测，也不能据此确定 frpc 故障或电脑已关机。", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
                item {
                    Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
                        Text(
                            if (finishing) "正在结束检查…" else if (running) "正在检查，最多约 25 秒…" else if (report.finished) "本次检查已结束 · ${report.elapsedMs} 毫秒" else "点击开始检查；需要已配对的手机身份。",
                            style = MaterialTheme.typography.bodyMedium,
                            modifier = Modifier.semantics { liveRegion = LiveRegionMode.Polite }
                        )
                        if (running) LinearProgressIndicator(Modifier.fillMaxWidth())
                        Button(
                            onClick = {
                                val check = ++generation
                                val capturedLoadTarget = loadTarget
                                started = SystemClock.elapsedRealtime()
                                report = DiagnosticReport.pending(routeIndex)
                                running = true
                                activeJob = scope.launch {
                                    try {
                                        val completed = diagnostics.run(origin, routeIndex, capturedLoadTarget) {
                                            if (generation == check) report = it
                                        }
                                        if (generation == check) {
                                            report = completed
                                            currentOnResult(completed)
                                        }
                                    } catch (_: CancellationException) {
                                        if (generation == check && currentCoroutineContext().isActive) {
                                            report = report.canceled(SystemClock.elapsedRealtime() - started)
                                            currentOnResult(report)
                                        }
                                    } finally {
                                        if (generation == check) {
                                            running = false
                                            activeJob = null
                                        }
                                    }
                                }
                            },
                            enabled = !running && !finishing,
                            modifier = Modifier.fillMaxWidth()
                        ) { Text(if (report.finished) "重新检查" else "开始检查") }
                        if (running) OutlinedButton(enabled = !finishing, onClick = { cancel(publish = true) }, modifier = Modifier.fillMaxWidth()) { Text("取消检查") }
                    }
                }
                items(report.stages, key = { it.stage }) { result -> DiagnosticStageCard(result) }
                item {
                    Text("DNS 最多等待 5 秒，认证与目标查询最多等待 20 秒。取消后不会继续更新结果；系统中已开始的网络调用可能短暂收尾，收尾期间不会叠加新检查。未检查、跳过、未知和取消均不代表成功。", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
        }
    }
}

@Composable
private fun DiagnosticStageCard(result: DiagnosticStageResult) {
    val color = when (result.outcome) {
        DiagnosticOutcome.FAILURE -> MaterialTheme.colorScheme.error
        DiagnosticOutcome.SUCCESS -> MaterialTheme.colorScheme.primary
        else -> MaterialTheme.colorScheme.onSurfaceVariant
    }
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(when (result.stage) {
                DiagnosticStage.DNS -> "1 · 系统 DNS"
                DiagnosticStage.HTTPS -> "2 · HTTPS 连接"
                DiagnosticStage.API -> "3 · 已认证目标 API"
                DiagnosticStage.ROUTER -> "4 · 路由器缓存状态"
                DiagnosticStage.TARGET -> "5 · 电脑缓存探测"
            }, style = MaterialTheme.typography.titleMedium)
            Text(when (result.outcome) {
                DiagnosticOutcome.NOT_RUN -> "尚未检查"
                DiagnosticOutcome.RUNNING -> "检查中"
                DiagnosticOutcome.SUCCESS -> "成功"
                DiagnosticOutcome.FAILURE -> "失败／不可达"
                DiagnosticOutcome.UNKNOWN -> "未知"
                DiagnosticOutcome.SKIPPED -> "已跳过"
                DiagnosticOutcome.CANCELED -> "已取消"
            }, style = MaterialTheme.typography.labelLarge, color = color)
            Text(diagnosticExplanation(result.detail), style = MaterialTheme.typography.bodyMedium)
            result.elapsedMs?.let { Text("本步骤耗时：$it 毫秒", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant) }
            if (result.stage == DiagnosticStage.ROUTER || result.stage == DiagnosticStage.TARGET) {
                if (result.outcome == DiagnosticOutcome.SUCCESS || result.outcome == DiagnosticOutcome.FAILURE || result.outcome == DiagnosticOutcome.UNKNOWN) {
                    Text(result.observedAt?.let { "服务器探测时间：${diagnosticTime.format(it)}（缓存）" } ?: "服务器未提供探测时间，状态时效未知。", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
        }
    }
}

private val diagnosticTime = DateTimeFormatter.ofPattern("yyyy-MM-dd HH:mm:ss").withZone(ZoneId.systemDefault())

private fun diagnosticExplanation(detail: DiagnosticDetail): String = when (detail) {
    DiagnosticDetail.WAITING -> "等待手动开始。"
    DiagnosticDetail.CHECKING -> "正在获取可观测结果。"
    DiagnosticDetail.SYSTEM_DNS_RESOLVED -> "系统解析器已返回地址；后续连接仍使用系统网络和 VPN 策略。"
    DiagnosticDetail.DNS_FAILED -> "DNS 未返回可用结果；不能据此判断服务器或电脑已关闭。"
    DiagnosticDetail.DNS_UNAVAILABLE -> "当前系统环境无法执行 DNS 检查。"
    DiagnosticDetail.HTTPS_RESPONSE_RECEIVED -> "已收到 HTTPS 响应；不代表 API 授权或路由器一定正常。"
    DiagnosticDetail.TLS_FAILED -> "TLS 安全连接失败；未跳过证书验证。"
    DiagnosticDetail.CONNECTION_FAILED -> "连接未能完成，请检查当前网络或稍后手动重试。"
    DiagnosticDetail.NETWORK_FAILED -> "出现其他网络错误；未记录原始异常或请求信息。"
    DiagnosticDetail.TIMED_OUT -> "在限定时间内未完成；超时不等于电脑已关机。"
    DiagnosticDetail.TARGET_QUERY_SUCCEEDED -> "已读取目标状态，未要求服务器重新探测，也未发送唤醒请求。"
    DiagnosticDetail.API_REJECTED -> "服务响应未满足本次查询要求；可能涉及授权、限流或服务状态。"
    DiagnosticDetail.RESPONSE_INVALID -> "未得到有效目标结果，无法进一步确认服务或目标状态。"
    DiagnosticDetail.AUTHORIZATION_REQUIRED -> "本机身份或授权不可用，请返回检查配对状态。"
    DiagnosticDetail.ROUTER_AVAILABLE -> "服务器报告路由器可达；本机未直接连接路由器。"
    DiagnosticDetail.ROUTER_UNAVAILABLE -> "服务器报告路由器不可达；原因尚不确定，不能直接断言 frpc 故障。"
    DiagnosticDetail.TARGET_REACHABLE -> "服务器缓存探测为可达，不保证此刻仍在线。"
    DiagnosticDetail.TARGET_UNREACHABLE -> "服务器缓存探测为不可达；也可能由网络、休眠或探测限制引起，不等于已关机。"
    DiagnosticDetail.TARGET_UNKNOWN -> "服务器尚无可用电脑探测结果。"
    DiagnosticDetail.NO_OBSERVATION -> "未获得足够证据，无法独立确认此阶段。"
    DiagnosticDetail.PREVIOUS_STAGE_FAILED -> "前序检查未完成，未执行或未获取此阶段结果。"
    DiagnosticDetail.CANCELED -> "本次检查已取消；不据此判断连接状态。"
    DiagnosticDetail.PREVIOUS_CHECK_FINISHING -> "上一次系统网络调用仍在收尾，请稍后手动重试；没有发出新请求。"
}
