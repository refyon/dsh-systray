// 主窗口可见性查询（Objective-C 侧实现，见 dsh_window_darwin.go 的说明）。
// 必须放在 *.m 文件里：cgo 只把 .m/.c 当 Objective-C/C 源编译，Go 文件里的
// Objective-C 片段需要 cgo 的 preamble（此处直接写 .m 更直观）。
#import <Cocoa/Cocoa.h>
#include <stdbool.h>

bool dsh_has_visible_window(void) {
    // 不用 [NSApp mainWindow]：Wails 主窗口在设置窗口隐藏后 mainWindow 可能为 nil，
    // 而我们要回答的是“本进程当前是否有可见窗口”（= 设置窗口是否已打开）。
    for (NSWindow *w in [NSApp windows]) {
        if ([w isVisible]) {
            return true;
        }
    }
    return false;
}
