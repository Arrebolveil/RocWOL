package wiki.hubu.rocwol.ui

import android.app.Activity
import android.content.Context
import android.os.Build
import androidx.compose.animation.core.Animatable
import androidx.compose.animation.core.FastOutSlowInEasing
import androidx.compose.animation.core.tween
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.ColorScheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.dynamicDarkColorScheme
import androidx.compose.material3.dynamicLightColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.lerp
import androidx.compose.ui.graphics.luminance
import androidx.compose.ui.platform.LocalConfiguration
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalView
import androidx.core.view.WindowCompat

enum class ThemeMode(val label: String) {
    LIGHT("浅色"), DARK("深色"), SYSTEM("跟随系统")
}

class ThemeSettings(context: Context) {
    private val preferences = context.getSharedPreferences("roc_wol_appearance", Context.MODE_PRIVATE)
    var mode by mutableStateOf(
        ThemeMode.entries.firstOrNull { it.name == preferences.getString("theme_mode", null) } ?: ThemeMode.SYSTEM
    )
        private set

    fun select(mode: ThemeMode) {
        preferences.edit().putString("theme_mode", mode.name).apply()
        this.mode = mode
    }
}

@Composable
fun rememberThemeSettings(): ThemeSettings {
    val context = LocalContext.current.applicationContext
    return remember(context) { ThemeSettings(context) }
}

// Tonal roles derived from the approved #3F51B5 seed; components consume roles only.
private val LightColors = lightColorScheme(
    primary = Color(0xFF4355B9), onPrimary = Color(0xFFFFFFFF),
    primaryContainer = Color(0xFFDFE0FF), onPrimaryContainer = Color(0xFF00105C),
    secondary = Color(0xFF5B5D72), onSecondary = Color(0xFFFFFFFF),
    secondaryContainer = Color(0xFFE0E1F9), onSecondaryContainer = Color(0xFF181A2C),
    tertiary = Color(0xFF77536D), onTertiary = Color(0xFFFFFFFF),
    tertiaryContainer = Color(0xFFFFD7F0), onTertiaryContainer = Color(0xFF2D1227),
    error = Color(0xFFBA1A1A), onError = Color(0xFFFFFFFF),
    errorContainer = Color(0xFFFFDAD6), onErrorContainer = Color(0xFF410002),
    background = Color(0xFFFBF8FF), onBackground = Color(0xFF1B1B21),
    surface = Color(0xFFFBF8FF), onSurface = Color(0xFF1B1B21),
    surfaceVariant = Color(0xFFE3E1EC), onSurfaceVariant = Color(0xFF45464F),
    outline = Color(0xFF767680), outlineVariant = Color(0xFFC6C5D0),
    inverseSurface = Color(0xFF303036), inverseOnSurface = Color(0xFFF3F0F7),
    inversePrimary = Color(0xFFBAC3FF), surfaceTint = Color(0xFF4355B9),
    surfaceBright = Color(0xFFFBF8FF), surfaceDim = Color(0xFFDBD9E0),
    surfaceContainerLowest = Color(0xFFFFFFFF), surfaceContainerLow = Color(0xFFF5F2FA),
    surfaceContainer = Color(0xFFEFEDF4), surfaceContainerHigh = Color(0xFFE9E7EF),
    surfaceContainerHighest = Color(0xFFE3E1E9), scrim = Color(0xFF000000)
)
private val DarkColors = darkColorScheme(
    primary = Color(0xFFBAC3FF), onPrimary = Color(0xFF0B247D),
    primaryContainer = Color(0xFF293D9F), onPrimaryContainer = Color(0xFFDFE0FF),
    secondary = Color(0xFFC4C5DD), onSecondary = Color(0xFF2D2F42),
    secondaryContainer = Color(0xFF444559), onSecondaryContainer = Color(0xFFE0E1F9),
    tertiary = Color(0xFFE6BAD8), onTertiary = Color(0xFF44263D),
    tertiaryContainer = Color(0xFF5D3C54), onTertiaryContainer = Color(0xFFFFD7F0),
    error = Color(0xFFFFB4AB), onError = Color(0xFF690005),
    errorContainer = Color(0xFF93000A), onErrorContainer = Color(0xFFFFDAD6),
    background = Color(0xFF121318), onBackground = Color(0xFFE3E1E9),
    surface = Color(0xFF121318), onSurface = Color(0xFFE3E1E9),
    surfaceVariant = Color(0xFF45464F), onSurfaceVariant = Color(0xFFC6C5D0),
    outline = Color(0xFF90909A), outlineVariant = Color(0xFF45464F),
    inverseSurface = Color(0xFFE3E1E9), inverseOnSurface = Color(0xFF303036),
    inversePrimary = Color(0xFF4355B9), surfaceTint = Color(0xFFBAC3FF),
    surfaceBright = Color(0xFF38393F), surfaceDim = Color(0xFF121318),
    surfaceContainerLowest = Color(0xFF0D0E13), surfaceContainerLow = Color(0xFF1B1B21),
    surfaceContainer = Color(0xFF1F1F25), surfaceContainerHigh = Color(0xFF292A2F),
    surfaceContainerHighest = Color(0xFF34343A), scrim = Color(0xFF000000)
)

@Composable
fun RocTheme(mode: ThemeMode, activity: Activity, content: @Composable () -> Unit) {
    val systemDark = isSystemInDarkTheme()
    val dark = when (mode) {
        ThemeMode.LIGHT -> false
        ThemeMode.DARK -> true
        ThemeMode.SYSTEM -> systemDark
    }
    val context = LocalContext.current
    val configuration = LocalConfiguration.current
    val targetColors = remember(context, configuration, dark) {
        if (Build.VERSION.SDK_INT >= 31) {
            if (dark) dynamicDarkColorScheme(context) else dynamicLightColorScheme(context)
        } else if (dark) DarkColors else LightColors
    }
    val colors = rememberAnimatedColorScheme(targetColors)
    val view = LocalView.current
    val insetsController = remember(activity, view) {
        WindowCompat.getInsetsController(activity.window, view)
    }
    // Choose the higher-contrast black/white icons against the displayed palette,
    // not the destination mode while its surface is still transitioning.
    val lightStatusBars = colors.surface.luminance() > 0.179f
    val lightNavigationBars = colors.surfaceContainer.luminance() > 0.179f
    LaunchedEffect(insetsController, lightStatusBars, lightNavigationBars) {
        insetsController.isAppearanceLightStatusBars = lightStatusBars
        insetsController.isAppearanceLightNavigationBars = lightNavigationBars
    }
    MaterialTheme(colorScheme = colors, content = content)
}

@Composable
private fun rememberAnimatedColorScheme(target: ColorScheme): ColorScheme {
    var colors by remember { mutableStateOf(target) }
    val progress = remember { Animatable(1f) }
    LaunchedEffect(target) {
        if (colors === target) return@LaunchedEffect
        // Retarget from the displayed frame so rapid switches never jump.
        val start = colors
        progress.snapTo(0f)
        progress.animateTo(1f, tween(durationMillis = 300, easing = FastOutSlowInEasing)) {
            colors = interpolateColorScheme(start, target, value)
        }
        colors = target
    }
    return colors
}

// Material3 1.4 has no ColorScheme lerp. Keep every role on one native Compose
// animation clock, including the fixed accents and all elevated surface tones.
private fun interpolateColorScheme(start: ColorScheme, end: ColorScheme, fraction: Float): ColorScheme {
    if (fraction <= 0f) return start
    if (fraction >= 1f) return end
    return ColorScheme(
        primary = lerp(start.primary, end.primary, fraction),
        onPrimary = lerp(start.onPrimary, end.onPrimary, fraction),
        primaryContainer = lerp(start.primaryContainer, end.primaryContainer, fraction),
        onPrimaryContainer = lerp(start.onPrimaryContainer, end.onPrimaryContainer, fraction),
        inversePrimary = lerp(start.inversePrimary, end.inversePrimary, fraction),
        secondary = lerp(start.secondary, end.secondary, fraction),
        onSecondary = lerp(start.onSecondary, end.onSecondary, fraction),
        secondaryContainer = lerp(start.secondaryContainer, end.secondaryContainer, fraction),
        onSecondaryContainer = lerp(start.onSecondaryContainer, end.onSecondaryContainer, fraction),
        tertiary = lerp(start.tertiary, end.tertiary, fraction),
        onTertiary = lerp(start.onTertiary, end.onTertiary, fraction),
        tertiaryContainer = lerp(start.tertiaryContainer, end.tertiaryContainer, fraction),
        onTertiaryContainer = lerp(start.onTertiaryContainer, end.onTertiaryContainer, fraction),
        background = lerp(start.background, end.background, fraction),
        onBackground = lerp(start.onBackground, end.onBackground, fraction),
        surface = lerp(start.surface, end.surface, fraction),
        onSurface = lerp(start.onSurface, end.onSurface, fraction),
        surfaceVariant = lerp(start.surfaceVariant, end.surfaceVariant, fraction),
        onSurfaceVariant = lerp(start.onSurfaceVariant, end.onSurfaceVariant, fraction),
        surfaceTint = lerp(start.surfaceTint, end.surfaceTint, fraction),
        inverseSurface = lerp(start.inverseSurface, end.inverseSurface, fraction),
        inverseOnSurface = lerp(start.inverseOnSurface, end.inverseOnSurface, fraction),
        error = lerp(start.error, end.error, fraction),
        onError = lerp(start.onError, end.onError, fraction),
        errorContainer = lerp(start.errorContainer, end.errorContainer, fraction),
        onErrorContainer = lerp(start.onErrorContainer, end.onErrorContainer, fraction),
        outline = lerp(start.outline, end.outline, fraction),
        outlineVariant = lerp(start.outlineVariant, end.outlineVariant, fraction),
        scrim = lerp(start.scrim, end.scrim, fraction),
        surfaceBright = lerp(start.surfaceBright, end.surfaceBright, fraction),
        surfaceDim = lerp(start.surfaceDim, end.surfaceDim, fraction),
        surfaceContainer = lerp(start.surfaceContainer, end.surfaceContainer, fraction),
        surfaceContainerHigh = lerp(start.surfaceContainerHigh, end.surfaceContainerHigh, fraction),
        surfaceContainerHighest = lerp(start.surfaceContainerHighest, end.surfaceContainerHighest, fraction),
        surfaceContainerLow = lerp(start.surfaceContainerLow, end.surfaceContainerLow, fraction),
        surfaceContainerLowest = lerp(start.surfaceContainerLowest, end.surfaceContainerLowest, fraction),
        primaryFixed = lerp(start.primaryFixed, end.primaryFixed, fraction),
        primaryFixedDim = lerp(start.primaryFixedDim, end.primaryFixedDim, fraction),
        onPrimaryFixed = lerp(start.onPrimaryFixed, end.onPrimaryFixed, fraction),
        onPrimaryFixedVariant = lerp(start.onPrimaryFixedVariant, end.onPrimaryFixedVariant, fraction),
        secondaryFixed = lerp(start.secondaryFixed, end.secondaryFixed, fraction),
        secondaryFixedDim = lerp(start.secondaryFixedDim, end.secondaryFixedDim, fraction),
        onSecondaryFixed = lerp(start.onSecondaryFixed, end.onSecondaryFixed, fraction),
        onSecondaryFixedVariant = lerp(start.onSecondaryFixedVariant, end.onSecondaryFixedVariant, fraction),
        tertiaryFixed = lerp(start.tertiaryFixed, end.tertiaryFixed, fraction),
        tertiaryFixedDim = lerp(start.tertiaryFixedDim, end.tertiaryFixedDim, fraction),
        onTertiaryFixed = lerp(start.onTertiaryFixed, end.onTertiaryFixed, fraction),
        onTertiaryFixedVariant = lerp(start.onTertiaryFixedVariant, end.onTertiaryFixedVariant, fraction)
    )
}
