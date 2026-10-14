package wiki.hubu.rocwol.ui

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.unit.dp
import wiki.hubu.rocwol.data.ServerProfile

@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun ServerSelector(model: WolViewModel, state: ScreenState, onManage: () -> Unit) {
    var open by remember { mutableStateOf(false) }
    TextButton(onClick = { open = true }, enabled = state.ready && !state.switchingLocked && !state.switching) {
        Text(state.activeProfile?.displayName ?: "选择服务器")
    }
    if (open) ModalBottomSheet(onDismissRequest = { open = false }) {
        Column(Modifier.fillMaxWidth().verticalScroll(rememberScrollState()).selectableGroup().padding(16.dp).navigationBarsPadding()) {
            Text("切换不会关闭其他服务器的管理授权", style = MaterialTheme.typography.bodyMedium)
            state.profiles.filter { it.removalMode == null }.forEach { profile ->
                Row(Modifier.fillMaxWidth().heightIn(min = 64.dp).selectable(
                    selected = state.activeProfileId == profile.profileId, role = Role.RadioButton,
                    enabled = !state.switchingLocked, onClick = { open = false; model.selectProfile(profile.profileId) }),
                    verticalAlignment = Alignment.CenterVertically) {
                    RadioButton(state.activeProfileId == profile.profileId, onClick = null)
                    Column(Modifier.weight(1f).padding(start = 12.dp)) {
                        Text(profile.displayName, style = MaterialTheme.typography.titleMedium)
                        state.authorizationMarkers[profile.profileId]?.takeIf { it.isNotEmpty() }?.let { Text(it, color = MaterialTheme.colorScheme.tertiary) }
                    }
                }
            }
            TextButton(onClick = { open = false; onManage() }) { Text("管理服务器") }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ServerProfilesScreen(model: WolViewModel, onBack: () -> Unit) {
    val state by model.state.collectAsState()
    var name by remember { mutableStateOf("") }
    var origin by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    var delete by remember { mutableStateOf<ServerProfile?>(null) }
    BackHandler(enabled = !busy, onBack = onBack)
    Scaffold(topBar = { TopAppBar(title = { Text("管理服务器") }, navigationIcon = { TextButton(onClick = onBack, enabled = !busy) { Text("返回") } }) }) { padding ->
        Box(Modifier.fillMaxSize().padding(padding), contentAlignment = Alignment.TopCenter) {
            Column(Modifier.widthIn(max = 840.dp).fillMaxSize().verticalScroll(rememberScrollState()).imePadding().padding(20.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
                Text("只添加你控制的 HTTPS 地址。备用线路必须属于同一服务实例；实例标识不能替代证书验证。切换服务器不会关闭已有授权。")
                state.profiles.forEach { profile ->
                    ServerProfileCard(profile, state, model, busy, onBusy = { busy = it }, onDelete = { delete = profile })
                }
                Card(Modifier.fillMaxWidth()) {
                    Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                        Text("添加你的服务器", style = MaterialTheme.typography.titleLarge)
                        OutlinedTextField(name, { name = it; error = null }, label = { Text("服务器名称") }, enabled = !busy, singleLine = true, modifier = Modifier.fillMaxWidth())
                        OutlinedTextField(origin, { origin = it; error = null }, label = { Text("主 HTTPS 地址") }, supportingText = { Text("例如 https://wol.example；不含路径、账号或查询参数") }, enabled = !busy, singleLine = true, isError = error != null, modifier = Modifier.fillMaxWidth())
                        error?.let { Text(it, color = MaterialTheme.colorScheme.error) }
                        Button(enabled = !busy && state.ready && name.isNotBlank() && origin.isNotBlank(), onClick = {
                            busy = true
                            model.addServer(name, origin) { failure -> busy = false; error = failure; if (failure == null) { name = ""; origin = "" } }
                        }) { Text(if (busy) "正在验证…" else "验证并保存") }
                        Text("验证只请求公开的服务信息，不发送配对码、公钥或令牌。保存后选择此服务器完成配对。", style = MaterialTheme.typography.bodySmall)
                    }
                }
            }
        }
    }
    delete?.let { profile ->
        AlertDialog(onDismissRequest = { delete = null }, title = { Text("删除「${profile.displayName}」？") },
            text = { Text("先尝试结束此服务器的管理授权，再清理本机身份和档案。其他服务器不受影响，历史将保留；本机移除不会撤销服务器设备。") },
            confirmButton = { TextButton(onClick = { delete = null; model.removeProfile(profile.profileId) }) { Text("删除服务器档案") } },
            dismissButton = { TextButton(onClick = { delete = null }) { Text("取消") } })
    }
}

@Composable
private fun ServerProfileCard(profile: ServerProfile, state: ScreenState, model: WolViewModel, busy: Boolean, onBusy: (Boolean) -> Unit, onDelete: () -> Unit) {
    var name by remember(profile.profileId, profile.displayName) { mutableStateOf(profile.displayName) }
    var origin by remember(profile.profileId) { mutableStateOf("") }
    var error by remember(profile.profileId) { mutableStateOf<String?>(null) }
    fun completed(failure: String?) { onBusy(false); error = failure; if (failure == null) origin = "" }
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text(profile.displayName + if (profile.profileId == state.activeProfileId) " · 当前" else "", style = MaterialTheme.typography.titleLarge)
            if (profile.removalMode != null) {
                Text("此服务器正在清理，不能联网。")
                TextButton(onClick = { model.removeProfile(profile.profileId, profile.removalMode, true) }) { Text("继续清理") }
            } else {
                state.authorizationMarkers[profile.profileId]?.takeIf { it.isNotEmpty() }?.let { Text(it, color = MaterialTheme.colorScheme.tertiary) }
                TextButton(enabled = !busy && !state.switchingLocked, onClick = { model.selectProfile(profile.profileId) }) { Text("选择此服务器") }
                OutlinedTextField(name, { name = it }, label = { Text("名称") }, enabled = !busy, modifier = Modifier.fillMaxWidth())
                TextButton(enabled = !busy && name.isNotBlank() && name != profile.displayName, onClick = { onBusy(true); model.renameServer(profile.profileId, name, ::completed) }) { Text("保存名称") }
                profile.origins.forEach { route ->
                    Text("线路 ${route.routeIndex}${if (route.origin == profile.selectedOrigin) " · 当前" else ""}${if (route.verifiedInstanceId == null) " · 待验证" else ""}", style = MaterialTheme.typography.labelLarge)
                    Text(route.origin, style = MaterialTheme.typography.bodyMedium)
                    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        if (profile.profileId == state.activeProfileId && route.origin != profile.selectedOrigin) TextButton(enabled = !busy && !state.switchingLocked && !state.switching, onClick = { model.selectOrigin(route.origin) }) { Text("验证并切换") }
                        TextButton(enabled = !busy && profile.origins.size > 1 && route.origin != profile.selectedOrigin, onClick = { onBusy(true); model.removeOrigin(profile.profileId, route.origin, ::completed) }) { Text("删除线路") }
                    }
                }
                if (profile.selectedOrigin == null) Text("旧身份保留但没有已确认线路。请输入原服务地址完成验证；不会猜测地址。", color = MaterialTheme.colorScheme.tertiary)
                OutlinedTextField(origin, { origin = it; error = null }, label = { Text(if (profile.selectedOrigin == null) "原服务 HTTPS 地址" else "备用 HTTPS 地址") }, enabled = !busy, singleLine = true, modifier = Modifier.fillMaxWidth(), isError = error != null)
                TextButton(enabled = !busy && origin.isNotBlank() && (profile.origins.size < 8 || profile.origins.any { it.origin == origin }), onClick = { onBusy(true); model.addOrigin(profile.profileId, origin, ::completed) }) { Text("验证并添加线路") }
                Text("删除当前线路前，请先选择该服务器并安全切换到另一条线路。最后一条线路不能删除。", style = MaterialTheme.typography.bodySmall)
                TextButton(enabled = !busy && !state.switchingLocked, onClick = onDelete) { Text("删除服务器档案") }
            }
            error?.let { Text(it, color = MaterialTheme.colorScheme.error) }
        }
    }
}
