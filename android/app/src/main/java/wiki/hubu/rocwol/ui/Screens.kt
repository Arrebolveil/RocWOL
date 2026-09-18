package wiki.hubu.rocwol.ui

import android.app.Activity
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.consumeWindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.isImeVisible
import androidx.compose.foundation.layout.safeDrawing
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.CheckCircle
import androidx.compose.material.icons.filled.Home
import androidx.compose.material.icons.filled.Info
import androidx.compose.material.icons.filled.Lock
import androidx.compose.material.icons.filled.Warning
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.platform.LocalFocusManager
import androidx.compose.ui.platform.LocalSoftwareKeyboardController
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import wiki.hubu.rocwol.data.Origins
import wiki.hubu.rocwol.data.Repository
import java.time.ZoneId
import java.time.format.DateTimeFormatter

@OptIn(androidx.compose.foundation.layout.ExperimentalLayoutApi::class)
@Composable
fun RocApp(activity: Activity, model: WolViewModel) {
    val state by model.state.collectAsStateWithLifecycle()
    val themeSettings = rememberThemeSettings()
    val imeVisible = WindowInsets.isImeVisible
    // Intentionally not saved: every activity launch opens Wake, never management.
    var tab by remember { mutableIntStateOf(0) }
    RocTheme(themeSettings.mode, activity) {
        Scaffold(modifier = Modifier.imePadding(), contentWindowInsets = WindowInsets.safeDrawing, bottomBar = {
            if (!imeVisible) NavigationBar {
                NavigationBarItem(selected = tab == 0, onClick = { tab = 0 }, icon = { Icon(Icons.Default.Home, contentDescription = null) }, label = { Text("唤醒") })
                NavigationBarItem(selected = tab == 1, onClick = { tab = 1 }, icon = { Icon(Icons.Default.Info, contentDescription = null) }, label = { Text("详细") })
            }
        }) { padding ->
            Column(Modifier.fillMaxSize().padding(padding).consumeWindowInsets(padding).verticalScroll(rememberScrollState()).padding(20.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
                Text(if (tab == 0) "网络唤醒" else "设备与线路", style = MaterialTheme.typography.headlineMedium)
                Text("当前线路：${if (state.origin == Origins.all.first()) "直连（默认）" else "Cloudflare 备用"}", style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.onSurfaceVariant)
                if (state.busy) Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    CircularProgressIndicator(Modifier.size(24.dp))
                    Text("正在处理，请稍候…")
                }
                state.message?.let { message ->
                    Card(Modifier.fillMaxWidth()) {
                        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                                Icon(Icons.Default.Info, contentDescription = null)
                                Text(message, style = MaterialTheme.typography.bodyMedium, modifier = Modifier.weight(1f))
                            }
                            TextButton(onClick = model::dismissMessage, modifier = Modifier.heightIn(min = 48.dp)) { Text("知道了") }
                        }
                    }
                }
                if (!state.paired && tab == 0) Pairing(activity, model, state)
                else if (tab == 0) WakePage(model, state)
                else DetailsPage(activity, model, state, themeSettings.mode, themeSettings::select)
                Spacer(Modifier.height(8.dp))
            }
        }
        if (state.confirmNewWake) AlertDialog(
            onDismissRequest = model::dismissConfirmation,
            icon = { Icon(Icons.Default.Warning, contentDescription = null) },
            title = { Text("上一次结果尚未确认") },
            text = { Text("上一次请求可能已经发包。建议先查询原请求。继续将明确创建新的请求 ID，并可能再次发送唤醒包；系统不会自动执行此操作。") },
            confirmButton = { TextButton(onClick = model::sendNewWake, modifier = Modifier.heightIn(min = 48.dp)) { Text("仍然发送新请求") } },
            dismissButton = { TextButton(onClick = model::dismissConfirmation, modifier = Modifier.heightIn(min = 48.dp)) { Text("返回查询") } }
        )
        if (state.confirmForget) AlertDialog(
            onDismissRequest = model::dismissConfirmation,
            title = { Text("清除本机身份？") },
            text = { Text("这将删除本机的两把密钥并需要新的配对码。它不会撤销服务器上的旧设备，也不会释放服务器名额。请先通过服务器 CLI 撤销旧记录。") },
            confirmButton = { TextButton(onClick = model::forget, modifier = Modifier.heightIn(min = 48.dp)) { Text("清除并重新配对") } },
            dismissButton = { TextButton(onClick = model::dismissConfirmation, modifier = Modifier.heightIn(min = 48.dp)) { Text("取消") } }
        )
    }
}

@Composable
private fun Pairing(activity: Activity, model: WolViewModel, state: ScreenState) {
    var code by remember { mutableStateOf("") }
    var name by remember { mutableStateOf(state.deviceName) }
    val focusManager = LocalFocusManager.current
    val keyboard = LocalSoftwareKeyboardController.current
    val codeFocus = remember { FocusRequester() }
    val canSubmit = !state.busy && code.matches(Regex("[A-Z2-7]{32}")) && name.isNotBlank() && name.codePointCount(0, name.length) <= 64
    val submit = {
        if (canSubmit) {
            keyboard?.hide()
            focusManager.clearFocus()
            val submitted = code
            code = ""
            model.pair(activity, submitted, name)
        }
    }
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
            Icon(Icons.Default.Lock, contentDescription = null)
            Text("配对此手机", style = MaterialTheme.typography.titleLarge)
            Text("从服务器本机 CLI 获取一次性配对码。首次配对必须完成系统认证，证明两把独立密钥均可使用。", style = MaterialTheme.typography.bodyMedium)
            OutlinedTextField(value = name, onValueChange = { name = it }, label = { Text("设备名称") }, singleLine = true, enabled = !state.busy,
                isError = name.codePointCount(0, name.length) > 64,
                keyboardOptions = KeyboardOptions(imeAction = ImeAction.Next),
                keyboardActions = KeyboardActions(onNext = { codeFocus.requestFocus() }),
                supportingText = { Text("最多 64 个字符，仅用于识别此手机") }, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(value = code, onValueChange = { code = it.filterNot(Char::isWhitespace).uppercase() }, label = { Text("一次性配对码") }, singleLine = true, enabled = !state.busy,
                visualTransformation = PasswordVisualTransformation(), keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Ascii, imeAction = ImeAction.Done),
                keyboardActions = KeyboardActions(onDone = { submit() }),
                supportingText = { Text("32 位，只在本机内存中使用，不会保存") }, modifier = Modifier.fillMaxWidth().focusRequester(codeFocus))
            Button(onClick = submit,
                enabled = canSubmit,
                modifier = Modifier.fillMaxWidth().heightIn(min = 56.dp)) { Text("系统认证并配对") }
            Text("前提：Android 11+、已设置安全锁屏，并支持逐次 CryptoObject 签名。认证能力不足时无法配对；管理密钥绝不降级。", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

@Composable
private fun WakePage(model: WolViewModel, state: ScreenState) {
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text(state.target?.name ?: "DESKTOP-IENBTJG", style = MaterialTheme.typography.titleLarge)
            StatusLine("服务器", state.server, state.server == "可达")
            StatusLine("路由器", when (state.target?.router) { "available" -> "可达"; "unavailable" -> "不可达"; else -> "未知" }, state.target?.router == "available")
            StatusLine("电脑探测", when (state.target?.reachability) { "reachable" -> "当前已在线"; "unreachable" -> "未响应（不等于已关机）"; else -> "未知" }, state.target?.reachability == "reachable")
            state.target?.checkedAt?.let { Text("检测时间：${formatTime(it)}", style = MaterialTheme.typography.bodySmall) }
            OutlinedButton(onClick = model::refresh, enabled = !state.busy, modifier = Modifier.fillMaxWidth().heightIn(min = 48.dp)) { Text("刷新状态") }
        }
    }
    val retryOriginal = state.wake?.let { !it.gatewayConfirmed && it.result in setOf("pending", "unknown") && it.createdAt.isAfter(java.time.Instant.now().minusSeconds(86_400)) } == true
    Button(onClick = model::requestWake, enabled = !state.busy, modifier = Modifier.fillMaxWidth().heightIn(min = 60.dp)) {
        Text(if (retryOriginal) "重试原唤醒请求（相同 ID）" else "唤醒电脑", style = MaterialTheme.typography.titleMedium)
    }
    LastResult(model, state)
    Text("仅发送唤醒包，不提供远程关机。网络或探测失败无法证明电脑已经关机。", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
}

@Composable
private fun StatusLine(label: String, value: String, positive: Boolean) {
    Row(horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
        Icon(if (positive) Icons.Default.CheckCircle else Icons.Default.Info, contentDescription = null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
        Column { Text(label, style = MaterialTheme.typography.labelMedium); Text(value, style = MaterialTheme.typography.bodyLarge) }
    }
}

@Composable
private fun LastResult(model: WolViewModel, state: ScreenState) {
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text("最近结果", style = MaterialTheme.typography.titleMedium)
            Text(state.resultText)
            if (state.waiting) Text("每 3 秒探测，最多等待 90 秒；离开前台停止轮询。", style = MaterialTheme.typography.bodySmall)
            state.wake?.let { wake ->
                Text("请求 ID：${wake.requestId}", style = MaterialTheme.typography.bodySmall)
                wake.sentAt?.let { Text("发送时间：${formatTime(it)}", style = MaterialTheme.typography.bodySmall) }
                if (wake.result == "pending" || wake.result == "unknown") {
                    OutlinedButton(onClick = model::queryOriginal, enabled = !state.busy, modifier = Modifier.fillMaxWidth().heightIn(min = 48.dp)) { Text("查询原请求（不发包）") }
                    if (!wake.gatewayConfirmed) Text("尚不能确认请求是否到达网关。明确重试会复用上方 ID，两条线路共用去重记录。", style = MaterialTheme.typography.bodySmall)
                }
            }
        }
    }
}

@Composable
private fun DetailsPage(activity: Activity, model: WolViewModel, state: ScreenState, themeMode: ThemeMode, onThemeChange: (ThemeMode) -> Unit) {
    Text("外观", style = MaterialTheme.typography.titleMedium)
    Column(Modifier.fillMaxWidth().selectableGroup()) {
        ThemeMode.entries.forEach { mode ->
            Row(Modifier.fillMaxWidth().heightIn(min = 56.dp).selectable(selected = themeMode == mode, role = Role.RadioButton, onClick = { onThemeChange(mode) }), verticalAlignment = Alignment.CenterVertically) {
                RadioButton(selected = themeMode == mode, onClick = null)
                Text(mode.label, modifier = Modifier.padding(start = 12.dp), style = MaterialTheme.typography.bodyLarge)
            }
        }
    }
    HorizontalDivider()
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text("固定目标（只读）", style = MaterialTheme.typography.titleMedium)
            Text("名称：${state.target?.name ?: "DESKTOP-IENBTJG"}")
            Text("MAC：${state.target?.mac ?: "B0:25:AA:83:C6:EF"}")
            Text("IP：${state.target?.ip ?: "192.168.2.100"}")
            Text("目标配置仅由部署端维护", style = MaterialTheme.typography.bodySmall)
        }
    }
    Text("连接线路", style = MaterialTheme.typography.titleMedium)
    Origins.all.forEachIndexed { index, origin ->
        Row(Modifier.fillMaxWidth().heightIn(min = 64.dp).selectable(selected = state.origin == origin, enabled = !state.busy, role = Role.RadioButton, onClick = { model.selectOrigin(origin) }).padding(vertical = 8.dp), verticalAlignment = Alignment.CenterVertically) {
            RadioButton(selected = state.origin == origin, onClick = null, enabled = !state.busy)
            Column(Modifier.padding(start = 12.dp).weight(1f)) {
                Text(if (index == 0) "直连（默认）" else "Cloudflare 备用", style = MaterialTheme.typography.bodyLarge)
                Text(origin, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
        }
    }
    Text("只手动切换，不自动重试或切换网络。Clash 保持原状；App 不会绕过系统 VPN。切换后重新静默认证，设备身份不变。", style = MaterialTheme.typography.bodySmall)
    LastResult(model, state)
    HorizontalDivider()
    Text("路由器管理", style = MaterialTheme.typography.titleLarge)
    Text("系统认证解锁硬件管理密钥，服务器验证签名后才放行浏览器；不是仅在本机显示认证弹窗。LuCI 仍需独立登录。", style = MaterialTheme.typography.bodyMedium)
    Text("下次授权时长：${state.sessionMinutes} 分钟", style = MaterialTheme.typography.titleMedium)
    Column(Modifier.fillMaxWidth().selectableGroup()) {
        Repository.sessionMinutesOptions.forEach { minutes ->
            Row(Modifier.fillMaxWidth().heightIn(min = 56.dp).selectable(selected = state.sessionMinutes == minutes, enabled = !state.busy, role = Role.RadioButton, onClick = { model.selectSessionMinutes(minutes) }), verticalAlignment = Alignment.CenterVertically) {
                RadioButton(selected = state.sessionMinutes == minutes, onClick = null, enabled = !state.busy)
                Text("$minutes 分钟${if (minutes == Repository.DEFAULT_SESSION_MINUTES) "（默认）" else ""}", modifier = Modifier.padding(start = 12.dp), style = MaterialTheme.typography.bodyLarge)
            }
        }
    }
    Text("设置仅对下一次系统认证授权生效，不会改变当前授权或自动续期。会话从浏览器兑换票据时开始计时。", style = MaterialTheme.typography.bodySmall)
    Text("返回 App 自动结束授权；也可手动结束。断网或 App 被强制停止时无法立即撤销，只能等待服务器到期。已加载的页面不会被远程擦除。", style = MaterialTheme.typography.bodySmall)
    Button(onClick = { model.manage(activity) }, enabled = state.paired && !state.busy, modifier = Modifier.fillMaxWidth().heightIn(min = 56.dp)) {
        Icon(Icons.Default.Lock, contentDescription = null, modifier = Modifier.padding(end = 8.dp))
        Text("系统认证并打开管理")
    }
    if (state.managementPending) {
        Text("管理授权尚未确认结束（本次 ${state.pendingSessionMinutes} 分钟）", color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.labelLarge)
        OutlinedButton(onClick = model::endManagement, enabled = !state.busy, modifier = Modifier.fillMaxWidth().heightIn(min = 56.dp)) {
            Text("结束管理授权")
        }
    }
    Text("遗失手机：在服务器执行设备撤销命令，立即失效其令牌和管理会话。App 不保存路由器密码。", style = MaterialTheme.typography.bodySmall)
    if (state.paired) TextButton(onClick = model::requestForget, enabled = !state.busy, modifier = Modifier.fillMaxWidth().heightIn(min = 48.dp)) { Text("清除本机身份并重新配对") }
}

private fun formatTime(value: java.time.Instant): String = DateTimeFormatter.ofPattern("yyyy-MM-dd HH:mm:ss").withZone(ZoneId.systemDefault()).format(value)
