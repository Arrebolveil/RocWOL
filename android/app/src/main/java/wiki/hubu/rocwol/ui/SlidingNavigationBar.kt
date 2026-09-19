package wiki.hubu.rocwol.ui

import androidx.compose.animation.animateColorAsState
import androidx.compose.animation.core.FastOutSlowInEasing
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.tween
import androidx.compose.foundation.background
import androidx.compose.foundation.indication
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.offset
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Home
import androidx.compose.material.icons.filled.Lock
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.NavigationBarDefaults
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.ripple
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.unit.IntOffset
import androidx.compose.ui.unit.dp

private enum class MainDestination(val title: String, val icon: ImageVector) {
    WAKE("唤醒", Icons.Default.Home),
    MANAGEMENT("管理", Icons.Default.Lock),
    SETTINGS("设置", Icons.Default.Settings)
}

@Composable
internal fun SlidingNavigationBar(selected: Int, onSelect: (Int) -> Unit) {
    val colors = MaterialTheme.colorScheme
    val position = animateFloatAsState(selected.toFloat(), tween(280, easing = FastOutSlowInEasing), label = "Navigation indicator position")
    val labelHeight = with(LocalDensity.current) { MaterialTheme.typography.labelMedium.lineHeight.toDp() }
    Surface(color = colors.surfaceContainer) {
        BoxWithConstraints(Modifier.fillMaxWidth().windowInsetsPadding(NavigationBarDefaults.windowInsets).height(64.dp + labelHeight)) {
            val itemWidth = maxWidth / MainDestination.entries.size
            Box(Modifier.offset {
                IntOffset(((itemWidth - 64.dp) / 2 + itemWidth * position.value).roundToPx(), 12.dp.roundToPx())
            }.size(64.dp, 32.dp).background(colors.secondaryContainer, CircleShape))
            Row(Modifier.fillMaxSize().selectableGroup()) {
                MainDestination.entries.forEachIndexed { index, destination ->
                    val source = remember { MutableInteractionSource() }
                    val iconColor by animateColorAsState(if (selected == index) colors.onSecondaryContainer else colors.onSurfaceVariant,
                        tween(180), label = "Navigation icon color")
                    val labelColor by animateColorAsState(if (selected == index) colors.onSurface else colors.onSurfaceVariant,
                        tween(180), label = "Navigation label color")
                    Column(
                        Modifier.weight(1f).fillMaxSize().selectable(selected == index, role = Role.Tab,
                            interactionSource = source, indication = null, onClick = { onSelect(index) }).padding(top = 12.dp, bottom = 16.dp),
                        horizontalAlignment = Alignment.CenterHorizontally
                    ) {
                        Box(Modifier.size(64.dp, 32.dp).clip(CircleShape).indication(source, ripple()), contentAlignment = Alignment.Center) {
                            Icon(destination.icon, contentDescription = null, tint = iconColor, modifier = Modifier.size(24.dp))
                        }
                        Spacer(Modifier.height(4.dp))
                        Text(destination.title, style = MaterialTheme.typography.labelMedium, color = labelColor, maxLines = 1)
                    }
                }
            }
        }
    }
}
