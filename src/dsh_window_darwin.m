// 主窗口可见性查询（Objective-C 侧实现，见 dsh_window_darwin.go 的说明）。
// 必须放在 *.m 文件里：cgo 只把 .m/.c 当 Objective-C/C 源编译，Go 文件里的
// Objective-C 片段需要 cgo 的 preamble（此处直接写 .m 更直观）。
#import <Cocoa/Cocoa.h>
#include <stdbool.h>

bool dsh_has_visible_window(void) {
    // 不用 [NSApp mainWindow]：Wails 主窗口在设置窗口隐藏后 mainWindow 可能为 nil，
    // 而我们要回答的是“本进程当前是否有可见窗口”（= 设置窗口是否已打开）。
    //
    // 两类必须排除的干扰窗口：
    //   1) 托盘状态栏窗口（NSStatusBarWindow——注意它不是 NSPanel）：菜单栏图标自身就是
    //      [NSApp windows] 里的一个常驻可见窗口，只要图标显示着就恒为 isVisible=YES。
    //      不过滤时本函数**恒返回 true**：托盘「设置」永远走「已打开 → 只置前、不重载内容」
    //      分支，前端再也收不到 ui:show-settings——凡是用进度视图顶掉设置页的流程（插件批量
    //      操作 / 重启后台服务 / 更新）结束后，用户就没有任何入口回到设置页，窗口一直停在
    //      「正在重启服务并校验…」上（2026-09-17 现场问题：插件更新完成后不退出重启中页面）。
    //   2) 面板类窗口（菜单 / 弹窗 / 文件选择器）。
    // 判据落在 canBecomeMainWindow 而不是窗口层级：置顶（截图模式 WindowSetAlwaysOnTop）会把
    // 设置窗口抬到 NSFloatingWindowLevel，按 level==NSNormalWindowLevel 过滤会把它一并滤掉。
    for (NSWindow *w in [NSApp windows]) {
        if (![w isVisible]) continue;
        if ([w isKindOfClass:NSClassFromString(@"NSStatusBarWindow")]) continue;
        if ([w isKindOfClass:[NSPanel class]]) continue;
        if (![w canBecomeMainWindow]) continue;
        return true;
    }
    return false;
}
