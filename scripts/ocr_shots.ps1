# 用 Windows 内置 OCR（zh-CN 语言包）把截图里的文字读出来，用于核对「脱敏 + 内容完整」：
#   - 读出的文字里不应出现本机真实路径 / 用户名 / 主机名；
#   - 每张图都应能读出预期关键词（页标题等），据此判断没有被裁掉、不是空白图。
# 用法: powershell -NoProfile -File scripts\ocr_shots.ps1 -Dir docs\shots -Out docs\.shots-tmp\ocr-zh.txt
param(
    [string]$Dir = "docs\shots",
    [string]$Out = ""
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$target = Join-Path $root $Dir

# System.Runtime.WindowsRuntime 在 PowerShell 5.1 里默认不加载，必须先显式加载。
# 注意：PowerShell 在**解析期**就绑定 [Type] 字面量，因此这里改用反射取类型，
# 不能写 [System.WindowsRuntimeSystemExtensions]（会报 Unable to find type）。
Add-Type -AssemblyName System.Runtime.WindowsRuntime | Out-Null
$wrAsm = [System.Reflection.Assembly]::LoadFrom((Join-Path $env:WINDIR 'Microsoft.NET\assembly\GAC_MSIL\System.Runtime.WindowsRuntime\v4.0_4.0.0.0__b77a5c561934e089\System.Runtime.WindowsRuntime.dll'))
$wrExt = $wrAsm.GetType('System.WindowsRuntimeSystemExtensions')
if ($null -eq $wrExt) { throw "找不到 System.WindowsRuntimeSystemExtensions" }

$asTaskMethods = $wrExt.GetMethods() |
    Where-Object { $_.Name -eq 'AsTask' -and $_.IsGenericMethod } |
    Where-Object { $_.GetParameters().Count -eq 1 -and $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncOperation`1' }
if (-not $asTaskMethods) { throw "找不到 AsTask(IAsyncOperation<T>) 重载" }
$asTaskGeneric = $asTaskMethods[0]

function Await($op, $resultType) {
    $t = $asTaskGeneric.MakeGenericMethod($resultType).Invoke($null, @($op))
    $t.Wait(-1) | Out-Null
    return $t.Result
}

[Windows.Media.Ocr.OcrEngine, Windows.Foundation, ContentType = WindowsRuntime] | Out-Null
[Windows.Graphics.Imaging.BitmapDecoder, Windows.Foundation, ContentType = WindowsRuntime] | Out-Null
[Windows.Storage.StorageFile, Windows.Foundation, ContentType = WindowsRuntime] | Out-Null
[Windows.Globalization.Language, Windows.Foundation, ContentType = WindowsRuntime] | Out-Null

$lang = New-Object Windows.Globalization.Language "zh-CN"
$engine = [Windows.Media.Ocr.OcrEngine]::TryCreateFromLanguage($lang)
if ($null -eq $engine) { throw "OCR 引擎不可用（缺少 zh-CN 语言包）" }

$lines = @()
foreach ($f in (Get-ChildItem -Path $target -File | Sort-Object Name)) {
    $file = Await ([Windows.Storage.StorageFile]::GetFileFromPathAsync($f.FullName)) ([Windows.Storage.StorageFile])
    $stream = Await ($file.OpenAsync([Windows.Storage.FileAccessMode]::Read)) ([Windows.Storage.Streams.IRandomAccessStream])
    $decoder = Await ([Windows.Graphics.Imaging.BitmapDecoder]::CreateAsync($stream)) ([Windows.Graphics.Imaging.BitmapDecoder])
    $bitmap = Await ($decoder.GetSoftwareBitmapAsync()) ([Windows.Graphics.Imaging.SoftwareBitmap])
    $result = Await ($engine.RecognizeAsync($bitmap)) ([Windows.Media.Ocr.OcrResult])
    $lines += "===== $($f.Name) ====="
    $lines += (($result.Lines | ForEach-Object { $_.Text }) -join "`n")
    $lines += ""
    $stream.Dispose()
}

$text = $lines -join "`n"
if ($Out) {
    $outPath = Join-Path $root $Out
    [IO.File]::WriteAllText($outPath, $text, [Text.UTF8Encoding]::new($false))
    Write-Host "wrote $outPath ($($text.Length) chars)"
} else {
    Write-Output $text
}
