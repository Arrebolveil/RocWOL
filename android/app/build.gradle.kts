import java.io.File

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
}

val signingRoot = File(System.getenv("LOCALAPPDATA") ?: System.getProperty("user.home"), "RocWol/signing")
fun signingFile(property: String, variable: String, name: String): File =
    File(providers.gradleProperty(property).orNull ?: System.getenv(variable) ?: File(signingRoot, name).path)
val releaseStore = signingFile("rocwol.keystore", "ROCWOL_KEYSTORE", "roc-wol.p12")
val storePasswordFile = signingFile("rocwol.storePasswordFile", "ROCWOL_STORE_PASSWORD_FILE", "store-password.txt")
val keyPasswordFile = signingFile("rocwol.keyPasswordFile", "ROCWOL_KEY_PASSWORD_FILE", "key-password.txt")
val signingReady = releaseStore.isFile && storePasswordFile.isFile && keyPasswordFile.isFile

android {
val directOrigin = providers.gradleProperty("rocwol.directOrigin").orElse("https://wol.hubu.wiki").get()
val fallbackOrigin = providers.gradleProperty("rocwol.fallbackOrigin").orElse("https://roc.hubu.wiki").get()
require(directOrigin in setOf("https://wol.hubu.wiki", "https://wol-direct.hubu.wiki"))
require(fallbackOrigin == "https://roc.hubu.wiki")
    namespace = "wiki.hubu.rocwol"
    compileSdk = 36
    defaultConfig {
        applicationId = "wiki.hubu.rocwol"
        minSdk = 30
        targetSdk = 36
        versionCode = 6
        versionName = "1.3.0"
        buildConfigField("String", "DIRECT_ORIGIN", "\"$directOrigin\"")
        buildConfigField("String", "FALLBACK_ORIGIN", "\"$fallbackOrigin\"")
    }
    signingConfigs {
        create("release") {
            if (signingReady) {
                storeFile = releaseStore
                storeType = "PKCS12"
                storePassword = storePasswordFile.readText().trimEnd('\r', '\n')
                keyAlias = "roc-wol"
                keyPassword = keyPasswordFile.readText().trimEnd('\r', '\n')
                require(!storePassword.isNullOrEmpty() && !keyPassword.isNullOrEmpty()) { "Release signing password files must not be empty" }
            }
        }
    }
    buildTypes {
        release {
            isDebuggable = false
            isMinifyEnabled = false
            signingConfig = signingConfigs.getByName("release")
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions { jvmTarget = "17" }
    buildFeatures { compose = true; buildConfig = true }
}

tasks.configureEach {
    if (name == "preReleaseBuild") {
        doFirst {
            check(signingReady) { "Protected RocWol release keystore and password files are required; unsigned/debug fallback is forbidden" }
        }
    }
}

dependencies {
    implementation(platform("androidx.compose:compose-bom:2025.12.01"))
    implementation("androidx.compose.material3:material3")
    implementation("androidx.compose.ui:ui")
    implementation("androidx.compose.foundation:foundation")
    implementation("androidx.compose.material:material-icons-core")
    implementation("androidx.activity:activity-compose:1.11.0")
    implementation("androidx.lifecycle:lifecycle-viewmodel-compose:2.9.4")
    implementation("androidx.lifecycle:lifecycle-runtime-compose:2.9.4")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.10.2")
}
