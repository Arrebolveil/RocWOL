package wiki.hubu.rocwol.ui

import android.app.Activity
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.FlowRow
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
import androidx.compose.foundation.selection.toggleable
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.CheckCircle
import androidx.compose.material.icons.filled.Home
import androidx.compose.material.icons.filled.Info
import androidx.compose.material.icons.filled.Lock
import androidx.compose.material.icons.filled.Warning
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.FilterChip
import androidx.compose.material3.Switch
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
import androidx.compose.runtime.LaunchedEffect
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
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LocalLifecycleOwner
import androidx.lifecycle.repeatOnLifecycle
import wiki.hubu.rocwol.data.Origins
import kotlinx.coroutines.delay
import java.time.Instant
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
    val wakeScroll = rememberScrollState()
    val managementScroll = rememberScrollState()
    val settingsScroll = rememberScrollState()
    LaunchedEffect(tab, state.paired) {
        if (tab == 1 && state.paired) model.refreshManagement()
    }
    RocTheme(themeSettings.mode, activity) {
        Scaffold(modifier = Modifier.imePadding(), contentWindowInsets = WindowInsets.safeDrawing, bottomBar = {
            if (!imeVisible) NavigationBar {
                NavigationBarItem(selected = tab == 0, onClick = { tab = 0 }, icon = { Icon(Icons.Default.Home, contentDescription = null) }, label = { Text("唤醒") })
                NavigationBarItem(selected = tab == 1, onClick = { tab = 1 }, icon = { Icon(Icons.Default.Lock, contentDescription = null) }, label = { Text("管理") })
                NavigationBarItem(selected = tab == 2, onClick = { tab = 2 }, icon = { Icon(Icons.Default.Settings, contentDescription = null) }, label = { Text("设置") })
            }
        }) { padding ->
            val scroll = when (tab) { 0 -> wakeScroll; 1 -> managementScroll; else -> settingsScroll }
            Column(Modifier.fillMaxSize().padding(padding).consumeWindowInsets(padding).verticalScroll(scroll).padding(20.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
                Text(when (tab) { 0 -> "网络唤醒"; 1 -> "路由器管理"; else -> "设置" }, style = MaterialTheme.typography.headlineMedium)
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
                else if (tab == 1) ManagementPage(activity, model, state)
                else SettingsPage(model, state, themeSettings.mode, themeSettings::select)
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
    Card(Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerHigh)) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text("固定目标", style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.onSurfaceVariant)
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
    Row(Modifier.fillMaxWidth().heightIn(min = 48.dp), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
        Icon(if (positive) Icons.Default.CheckCircle else Icons.Default.Info, contentDescription = null,
            tint = if (positive) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.size(20.dp))
        Text(label, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.weight(1f))
        Text(value, style = MaterialTheme.typography.bodyLarge, modifier = Modifier.weight(1.7f))
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
private fun SettingsPage(model: WolViewModel, state: ScreenState, themeMode: ThemeMode, onThemeChange: (ThemeMode) -> Unit) {
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text("外观", style = MaterialTheme.typography.titleMedium)
            Column(Modifier.fillMaxWidth().selectableGroup()) {
                ThemeMode.entries.forEach { mode ->
                    Row(Modifier.fillMaxWidth().heightIn(min = 56.dp).selectable(selected = themeMode == mode, role = Role.RadioButton, onClick = { onThemeChange(mode) }), verticalAlignment = Alignment.CenterVertically) {
                        RadioButton(selected = themeMode == mode, onClick = null)
                        Text(mode.label, modifier = Modifier.padding(start = 12.dp), style = MaterialTheme.typography.bodyLarge)
                    }
                }
            }
        }
    }
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text("连接线路", style = MaterialTheme.typography.titleMedium)
            Column(Modifier.selectableGroup()) {
                Origins.all.forEachIndexed { index, origin ->
                    Row(Modifier.fillMaxWidth().heightIn(min = 72.dp).selectable(selected = state.origin == origin, enabled = !state.busy, role = Role.RadioButton, onClick = { model.selectOrigin(origin) }), verticalAlignment = Alignment.CenterVertically) {
                        RadioButton(selected = state.origin == origin, onClick = null, enabled = !state.busy)
                        Column(Modifier.padding(start = 12.dp).weight(1f)) {
                            Text(if (index == 0) "直连（默认）" else "Cloudflare 备用", style = MaterialTheme.typography.bodyLarge)
                            Text(origin, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                        }
                    }
                }
            }
            Text("只手动切换，不自动补发唤醒请求。切换线路前会结束本机管理授权；App 不会绕过系统 VPN。", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text("固定目标", style = MaterialTheme.typography.titleMedium)
            Text(state.target?.name ?: "DESKTOP-IENBTJG", style = MaterialTheme.typography.bodyLarge)
            Text("MAC  ${state.target?.mac ?: "B0:25:AA:83:C6:EF"}", style = MaterialTheme.typography.bodyMedium)
            Text("IP  ${state.target?.ip ?: "192.168.2.100"}", style = MaterialTheme.typography.bodyMedium)
            Text("目标由部署端维护，App 内只读。", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
    if (state.paired) {
        Text("此手机 · ${state.deviceName}", style = MaterialTheme.typography.titleMedium)
        TextButton(onClick = model::requestForget, enabled = !state.busy, modifier = Modifier.heightIn(min = 48.dp)) { Text("清除本机身份并重新配对") }
    }
}

@OptIn(androidx.compose.foundation.layout.ExperimentalLayoutApi::class)
@Composable
private fun ManagementPage(activity: Activity, model: WolViewModel, state: ScreenState) {
    var now by remember { mutableStateOf(Instant.now()) }
    var confirmAllDevices by remember { mutableStateOf(false) }
    val authorization = state.management
    val lifecycleOwner = LocalLifecycleOwner.current
    LaunchedEffect(authorization?.expiresAt, lifecycleOwner) {
        lifecycleOwner.lifecycle.repeatOnLifecycle(Lifecycle.State.STARTED) {
            now = Instant.now()
            val expires = authorization?.expiresAt ?: return@repeatOnLifecycle
            while (now.isBefore(expires)) {
                delay(1_000)
                now = Instant.now()
            }
            model.refreshManagement()
        }
    }
    val active = authorization?.expiresAt?.isAfter(now) == true
    val uncertain = !state.managementKnown && state.managementPending
    val cancelMode = active || uncertain
    val remaining = authorization?.let { java.time.Duration.between(now, it.expiresAt).seconds.coerceAtLeast(0) } ?: 0
    Card(Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerHigh)) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
            Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.SpaceBetween) {
                Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    Icon(if (active) Icons.Default.CheckCircle else Icons.Default.Lock, contentDescription = null,
                        tint = if (active) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant)
                    Text("管理授权", style = MaterialTheme.typography.titleMedium)
                }
                TextButton(onClick = model::refreshManagement, enabled = state.paired && !state.busy) { Text("刷新") }
            }
            Text(
                when { !state.paired -> "尚未配对"; uncertain -> "授权状态待确认"; active -> "已授权"; !state.managementKnown -> "尚未确认"; authorization != null -> "授权已到期"; else -> "未授权" },
                style = MaterialTheme.typography.headlineSmall
            )
            if (active) {
                Text("剩余 ${remaining / 60} 分 ${remaining % 60} 秒", style = MaterialTheme.typography.titleLarge, color = MaterialTheme.colorScheme.primary)
                Text(if (authorization.allowAllDevices) "所有设备可访问 · 仍需 LuCI 登录" else "仅已授权浏览器可访问", style = MaterialTheme.typography.bodyMedium)
                Text("到期 ${formatTime(authorization.expiresAt)}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            } else {
                Text(
                    when { !state.paired -> "先到“唤醒”页面配对此手机。"; uncertain -> "未能确认服务器状态。可以刷新，或直接取消本机授权。"; else -> "完成系统认证后开始计时。返回 App 不会结束授权。" },
                    style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant
                )
            }
            Button(
                onClick = { if (cancelMode) model.endManagement() else model.authorizeManagement(activity) },
                enabled = state.paired && !state.busy,
                colors = if (cancelMode) ButtonDefaults.buttonColors(
                    containerColor = MaterialTheme.colorScheme.error, contentColor = MaterialTheme.colorScheme.onError,
                    disabledContainerColor = MaterialTheme.colorScheme.error.copy(alpha = 0.55f),
                    disabledContentColor = MaterialTheme.colorScheme.onError
                ) else ButtonDefaults.buttonColors(),
                modifier = Modifier.fillMaxWidth().heightIn(min = 56.dp)
            ) {
                Text(if (cancelMode) "取消授权" else "授权管理", style = MaterialTheme.typography.titleMedium)
            }
            if (active) {
                FilledTonalButton(onClick = { model.openManagement(activity) }, enabled = !state.busy, modifier = Modifier.fillMaxWidth().heightIn(min = 48.dp)) {
                    Text("打开浏览器")
                }
                if (authorization?.allowAllDevices == true) {
                    Text("其他设备可在授权期间访问：", style = MaterialTheme.typography.bodySmall)
                    SelectionContainer { Text("${authorization.origin}/cgi-bin/luci/", style = MaterialTheme.typography.bodySmall) }
                }
            }
        }
    }
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text("授权设置", style = MaterialTheme.typography.titleMedium)
            Text("授权时长", style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.onSurfaceVariant)
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Repository.sessionMinutesOptions.forEach { minutes ->
                    FilterChip(selected = state.sessionMinutes == minutes, onClick = { model.selectSessionMinutes(minutes) },
                        enabled = !state.busy, label = { Text("${minutes}分钟") })
                }
            }
            HorizontalDivider()
            ManagementSwitch("自动打开浏览器", "认证成功后，直接进入默认浏览器。", state.autoOpenBrowser, !state.busy, model::setAutoOpenBrowser)
            HorizontalDivider()
            ManagementSwitch("允许所有设备访问", "临时向所有设备开放此线路的 LuCI 登录页。", state.allowAllDevices, !state.busy) {
                if (it) confirmAllDevices = true else model.setAllowAllDevices(false)
            }
            if (state.allowAllDevices) Text("开启后，知道管理网址的任何人都能访问登录页。路由器密码仍然必需。", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
            Text("时长和访问范围只对下次授权生效，不延长或改变当前授权。", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
    Text("取消授权会关闭本机发起的管理权限，其他手机的授权不受影响。离线时无法保证立即取消，到期由服务器关闭；已加载的网页不会被擦除。", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
    if (confirmAllDevices) AlertDialog(
        onDismissRequest = { confirmAllDevices = false },
        icon = { Icon(Icons.Default.Warning, contentDescription = null) },
        title = { Text("允许所有设备访问？") },
        text = { Text("下次授权期间，任何能访问此管理网址的人都能进入 LuCI 登录页，不需要 App 配对。仍需路由器账号密码。\n\n到期或取消本机授权后关闭这次访问权限。") },
        confirmButton = { TextButton(onClick = { confirmAllDevices = false; model.setAllowAllDevices(true) }) { Text("允许") } },
        dismissButton = { TextButton(onClick = { confirmAllDevices = false }) { Text("保持关闭") } }
    )
}

@Composable
private fun ManagementSwitch(title: String, description: String, checked: Boolean, enabled: Boolean, onChange: (Boolean) -> Unit) {
    Row(Modifier.fillMaxWidth().heightIn(min = 72.dp).toggleable(value = checked, enabled = enabled, role = Role.Switch, onValueChange = onChange),
        verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(16.dp)) {
        Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text(title, style = MaterialTheme.typography.bodyLarge)
            Text(description, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Switch(checked = checked, onCheckedChange = null, enabled = enabled)
    }
}

private fun formatTime(value: java.time.Instant): String = DateTimeFormatter.ofPattern("yyyy-MM-dd HH:mm:ss").withZone(ZoneId.systemDefault()).format(value)
