package wiki.hubu.rocwol.ui

import android.content.ActivityNotFoundException
import android.content.Intent
import android.net.Uri
import android.os.Build
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
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.Card
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.launch
import wiki.hubu.rocwol.BuildConfig

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AboutScreen(onBack: () -> Unit) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val snackbar = remember { SnackbarHostState() }
    BackHandler(onBack = onBack)
    Scaffold(
        topBar = {
            TopAppBar(title = { Text("关于") }, navigationIcon = {
                IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "返回") }
            })
        },
        snackbarHost = { SnackbarHost(snackbar) }
    ) { padding ->
        Box(Modifier.fillMaxSize().padding(padding), contentAlignment = Alignment.TopCenter) {
            LazyColumn(
                modifier = Modifier.widthIn(max = 840.dp).fillMaxSize(),
                contentPadding = PaddingValues(20.dp),
                verticalArrangement = Arrangement.spacedBy(16.dp)
            ) {
                item {
                    AboutCard("网络唤醒") {
                        Text("版本 ${BuildConfig.VERSION_NAME}（${BuildConfig.VERSION_CODE}）", style = MaterialTheme.typography.bodyLarge)
                        Text("Android ${Build.VERSION.RELEASE} · API ${Build.VERSION.SDK_INT}", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                        Text("安全地管理已配对设备并手动唤醒固定目标。", style = MaterialTheme.typography.bodyMedium)
                    }
                }
                item {
                    AboutCard("客户端协议支持") {
                        Text("pairV1 · manageV2 · device-adminV1", style = MaterialTheme.typography.bodyLarge)
                        Text("以上为本客户端实现的协议，不代表已检测或确认服务器正在运行的协议版本。实际可用功能以服务器响应与授权为准。", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
                item {
                    AboutCard("隐私与本机日志") {
                        Text("操作日志和本机诊断记录保存在应用私有空间，不自动上传；日志不包含令牌、签名、私钥、请求头或原始异常信息。", style = MaterialTheme.typography.bodyMedium)
                        Text("只有你主动导出或分享时，日志才会交给所选应用。分享前请检查内容；清理日志不会删除接收方已保存的副本。", style = MaterialTheme.typography.bodyMedium)
                        Text("正常功能会与所选服务器通信。连接诊断使用系统 DNS 与已认证的只读目标查询；不自动重试唤醒，也不绕过系统 VPN。", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
                item {
                    AboutCard("手动查看更新") {
                        Text("在浏览器打开 GitHub 发布页面，自行查看版本说明与可用下载。本应用不会自动下载或安装更新，也不内置 GitHub 凭据。", style = MaterialTheme.typography.bodyMedium)
                        Text("若仓库或发布内容为私有，你需要在浏览器中使用有权限的账号登录；打开链接不保证能够访问。", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                        Text(RELEASE_PAGE, style = MaterialTheme.typography.bodySmall)
                        OutlinedButton(onClick = {
                            val message = try {
                                context.startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(RELEASE_PAGE)).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
                                null
                            } catch (_: ActivityNotFoundException) {
                                "未找到可打开链接的浏览器，请手动访问上方发布地址。"
                            } catch (_: SecurityException) {
                                "系统不允许打开浏览器，请手动访问上方发布地址。"
                            }
                            if (message != null) scope.launch { snackbar.showSnackbar(message) }
                        }, modifier = Modifier.fillMaxWidth()) { Text("打开发布页面") }
                    }
                }
            }
        }
    }
}

@Composable
private fun AboutCard(title: String, content: @Composable () -> Unit) {
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text(title, style = MaterialTheme.typography.titleLarge)
            content()
        }
    }
}

private const val RELEASE_PAGE = "https://github.com/Arrebolveil/RocWOL/releases"
