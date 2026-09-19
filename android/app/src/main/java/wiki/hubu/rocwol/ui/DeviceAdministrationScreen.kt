package wiki.hubu.rocwol.ui

import android.app.Activity
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.LiveRegionMode
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.liveRegion
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.withContext
import kotlinx.coroutines.launch
import wiki.hubu.rocwol.data.AdministeredDevice
import wiki.hubu.rocwol.data.ApiException
import wiki.hubu.rocwol.data.DeviceAdministration
import wiki.hubu.rocwol.data.DeviceAdministrationAccess
import wiki.hubu.rocwol.data.Repository
import wiki.hubu.rocwol.data.administrationFailureOutcome
import wiki.hubu.rocwol.data.journal.JournalOutcome
import wiki.hubu.rocwol.security.AuthenticationCancelled
import wiki.hubu.rocwol.security.AuthenticationUnavailable
import wiki.hubu.rocwol.security.IdentityLost
import java.time.ZoneId
import java.time.format.DateTimeFormatter

@Composable
fun DeviceAdministrationScreen(activity: Activity, repository: Repository, onIdentityRevoked: () -> Unit, onBack: () -> Unit) {
    val administration = remember(repository) { DeviceAdministration(repository) }
    val scope = rememberCoroutineScope()
    val identityRevoked by rememberUpdatedState(onIdentityRevoked)
    var access by remember(repository) { mutableStateOf<DeviceAdministrationAccess?>(null) }
    var devices by remember(repository) { mutableStateOf<List<AdministeredDevice>?>(null) }
    var busy by remember { mutableStateOf(true) }
    var message by remember { mutableStateOf<String?>(null) }
    var error by remember { mutableStateOf(false) }
    var confirmation by remember { mutableStateOf<AdministeredDevice?>(null) }
    val dateFormat = remember { DateTimeFormatter.ofPattern("yyyy年M月d日 HH:mm").withZone(ZoneId.systemDefault()) }

    suspend fun handleFailure(failure: Exception, revoking: Boolean = false) {
        if (failure is CancellationException) throw failure
        error = failure !is AuthenticationCancelled
        message = when (failure) {
            is AuthenticationCancelled -> "已取消系统验证，未提交设备管理操作。"
            is AuthenticationUnavailable -> "系统验证暂不可用。请检查锁屏密码或生物识别设置，再手动重试。"
            is IdentityLost -> {
                devices = null
                repository.forgetIdentity()
                identityRevoked()
                "此手机身份已失效，请重新配对。"
            }
            is ApiException -> when {
                revoking && administrationFailureOutcome(failure, submitted = true) == JournalOutcome.UNKNOWN ->
                    "未能确认撤销结果，不会自动重试。请重新验证并刷新列表确认设备状态。"
                failure.code == "device_administration_denied" -> {
                    access = repository.deviceId?.let { DeviceAdministrationAccess(it, false) }
                    devices = null
                    "此手机没有设备管理权限。请联系服务器管理员通过本地管理命令授予权限。"
                }
                failure.status == 429 -> "操作过于频繁。请至少等待 ${failure.retryAfter ?: 60} 秒后手动重试。"
                failure.status == 409 -> "本次验证已过期或已使用，请重新发起操作并验证。"
                failure.status == 404 && revoking -> "该设备已不在有效设备列表中，请重新验证后刷新列表。"
                failure.status == 401 -> {
                    access = null
                    devices = null
                    "身份凭据已失效。请重新检查权限；若身份已撤销，需要重新配对。"
                }
                failure.status == 403 -> "服务器拒绝本次验证，未执行操作。请检查当前线路后重试。"
                revoking -> "未能确认撤销结果，不会自动重试。请重新验证并刷新列表确认设备状态。"
                else -> "暂时无法读取设备管理信息，请稍后重试。"
            }
            else -> if (revoking) "未能确认撤销结果，不会自动重试。请重新验证并刷新列表确认设备状态。"
                else "无法连接服务器或读取安全响应，请检查当前线路后重试。"
        }
    }

    suspend fun checkAccess() {
        busy = true
        message = null
        error = false
        try { access = administration.access() }
        catch (failure: Exception) { handleFailure(failure) }
        finally { busy = false }
    }

    LaunchedEffect(administration) { checkAccess() }

    Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.background) {
        Box(Modifier.fillMaxSize().safeDrawingPadding(), contentAlignment = Alignment.TopCenter) {
            Column(
                Modifier.widthIn(max = 720.dp).fillMaxSize().verticalScroll(rememberScrollState()).padding(20.dp),
                verticalArrangement = Arrangement.spacedBy(16.dp)
            ) {
                Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
                    TextButton(onClick = onBack) { Text("返回") }
                    Text("已配对设备", style = MaterialTheme.typography.headlineSmall, modifier = Modifier.semantics { heading() })
                }
                Text("设备管理是独立权限，与唤醒权限、路由器管理授权分开。查看列表和每次撤销都需要系统验证。",
                    color = MaterialTheme.colorScheme.onSurfaceVariant)
                if (busy) LinearProgressIndicator(Modifier.fillMaxWidth())
                message?.let {
                    Card(colors = CardDefaults.cardColors(containerColor = if (error) MaterialTheme.colorScheme.errorContainer else MaterialTheme.colorScheme.secondaryContainer)) {
                        Text(it, Modifier.padding(16.dp).semantics { liveRegion = LiveRegionMode.Polite },
                            color = if (error) MaterialTheme.colorScheme.onErrorContainer else MaterialTheme.colorScheme.onSecondaryContainer)
                    }
                }
                when (access?.deviceAdministration) {
                    null -> if (!busy) {
                        OutlinedButton(onClick = { scope.launch { checkAccess() } }) { Text("检查本机权限") }
                    }
                    false -> {
                        Card {
                            Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
                                Text("尚无设备管理权限", style = MaterialTheme.typography.titleMedium)
                                Text("配对不会自动授予此权限。请将下面的本机设备 ID 提供给服务器管理员，由其通过受保护的本地 Unix socket 授权。普通唤醒凭据或 LuCI 管理授权不能代替此权限。")
                                SelectionContainer { Text(access?.deviceId.orEmpty(), style = MaterialTheme.typography.bodySmall) }
                                Text("管理员可使用：", style = MaterialTheme.typography.labelLarge)
                                SelectionContainer {
                                    Text("roc-wol devices admin --id ${access?.deviceId.orEmpty()} --enabled=true", style = MaterialTheme.typography.bodySmall)
                                }
                            }
                        }
                        OutlinedButton(enabled = !busy, onClick = { scope.launch { checkAccess() } }) { Text("重新检查权限") }
                    }
                    true -> {
                        Button(enabled = !busy, onClick = {
                            scope.launch {
                                busy = true
                                message = null
                                error = false
                                try {
                                    devices = administration.list(activity)
                                    message = "已验证并更新设备列表。"
                                } catch (failure: Exception) { handleFailure(failure) }
                                finally { busy = false }
                            }
                        }) { Text(if (devices == null) "验证并查看设备" else "验证并刷新列表") }
                        if (devices == null) Text("点击上方按钮开始系统验证。不会自动弹出或反复请求验证。", color = MaterialTheme.colorScheme.onSurfaceVariant)
                        devices?.let { list ->
                            Text("有效设备 ${list.size} / 5", style = MaterialTheme.typography.titleMedium)
                            list.forEach { device ->
                                Card(Modifier.fillMaxWidth()) {
                                    Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                                        Text(device.deviceName, style = MaterialTheme.typography.titleLarge)
                                        if (device.current) Text("当前手机", color = MaterialTheme.colorScheme.primary, style = MaterialTheme.typography.labelLarge)
                                        Text(if (device.deviceAdministration) "具有设备管理权限" else "无设备管理权限", color = MaterialTheme.colorScheme.onSurfaceVariant)
                                        Text("配对时间：${dateFormat.format(device.createdAt)}", style = MaterialTheme.typography.bodyMedium)
                                        SelectionContainer { Text(device.deviceId, style = MaterialTheme.typography.bodySmall) }
                                        OutlinedButton(enabled = !busy, onClick = { confirmation = device },
                                            colors = ButtonDefaults.outlinedButtonColors(contentColor = MaterialTheme.colorScheme.error)) {
                                            Text(if (device.current) "撤销当前手机" else "撤销此设备")
                                        }
                                    }
                                }
                            }
                        }
                        Text("撤销会立即使该设备的唤醒凭据、浏览器管理授权和正在进行的请求失效，无法撤回。需要恢复时必须重新配对。服务器本地管理命令始终可用于权限恢复。",
                            color = MaterialTheme.colorScheme.onSurfaceVariant, style = MaterialTheme.typography.bodySmall)
                    }
                }
            }
        }
    }

    confirmation?.let { target ->
        AlertDialog(
            onDismissRequest = { confirmation = null },
            title = { Text(if (target.current) "撤销当前手机？" else "撤销「${target.deviceName}」？") },
            text = {
                Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
                    Text("设备：${target.deviceName}${if (target.current) "（当前手机）" else ""}")
                    Text(target.deviceId, style = MaterialTheme.typography.bodySmall)
                    Text(if (target.current) "确认后还需系统验证。成功撤销将清除此手机的本地身份并退出配对状态；包括本机在内的该设备授权和请求会立即失效。即使这是最后一台管理手机，也只能由服务器管理员重新配对和授权恢复。"
                        else "确认后还需系统验证。该设备将立即失去唤醒与浏览器管理权限，正在进行的请求也会被取消。此操作无法撤回。")
                }
            },
            confirmButton = {
                TextButton(colors = ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error), onClick = {
                    confirmation = null
                    scope.launch {
                        var selfRevoked = false
                        busy = true
                        message = null
                        error = false
                        try {
                            val result = administration.revoke(activity, target)
                            if (result.current) {
                                selfRevoked = true
                                withContext(NonCancellable) {
                                    try { repository.forgetIdentity() }
                                    finally { identityRevoked() }
                                }
                            } else {
                                devices = devices?.filterNot { it.deviceId == result.deviceId }
                                message = "已撤销「${target.deviceName}」，该设备的凭据与管理授权已失效。"
                            }
                        } catch (failure: Exception) {
                            if (failure is CancellationException) throw failure
                            if (selfRevoked) {
                                error = true
                                message = "此手机已在服务器撤销，本地身份清理未完成。请重新打开应用并清除本机身份。"
                            } else handleFailure(failure, revoking = true)
                        }
                        finally { busy = false }
                    }
                }) { Text("验证并撤销") }
            },
            dismissButton = { TextButton(onClick = { confirmation = null }) { Text("保留设备") } }
        )
    }
}
