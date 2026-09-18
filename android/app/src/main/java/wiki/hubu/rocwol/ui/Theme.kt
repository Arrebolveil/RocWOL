package wiki.hubu.rocwol.ui

import android.app.Activity
import android.content.Context
import android.os.Build
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.dynamicDarkColorScheme
import androidx.compose.material3.dynamicLightColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.SideEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.graphics.Color
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
    val view = LocalView.current
    SideEffect {
        WindowCompat.getInsetsController(activity.window, view).apply {
            isAppearanceLightStatusBars = !dark
            isAppearanceLightNavigationBars = !dark
        }
    }
    val colors = remember(context, dark) {
        if (Build.VERSION.SDK_INT >= 31) {
            if (dark) dynamicDarkColorScheme(context) else dynamicLightColorScheme(context)
        } else if (dark) DarkColors else LightColors
    }
    MaterialTheme(colorScheme = colors, content = content)
}
