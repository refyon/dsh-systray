// 设置窗口原生实现（macOS Cocoa）：左侧分类列表（常规 / 关于 / 日志 / 导出 / 导入）+ 右侧内容面板。
// 由 cgo 编译（settings_darwin.go 中 #cgo darwin CFLAGS: -x objective-c -fobjc-arc）。
#import <Cocoa/Cocoa.h>
#import <UniformTypeIdentifiers/UniformTypeIdentifiers.h>
#import <dispatch/dispatch.h>
#import <stdlib.h>
#import <string.h>

// Go 侧回调（settings_darwin.go //export）
extern void dshSettingsGoAutostartToggled(int on);
extern void dshSettingsGoCheckUpdate(void);
// 返回 malloc 的日志文本 C 字符串（调用方 free）；which=0 app.log / 1 server.log
char* dshSettingsGoLoadLog(int which);
// 返回当前所选日志文件完整路径 C 字符串（调用方 free）
char* dshSettingsGoLogPath(int which);
// 清空日志文件（which=0 app.log / 1 server.log）
void dshSettingsGoClearLog(int which);
// 重启后台服务（异步执行）
void dshSettingsGoRestartService(void);
// 返回后台服务状态 C 字符串（"运行中"/"未运行"，调用方 free）
char* dshSettingsGoServiceState(void);
// 导出（阻塞）：返回 JSON C 字符串（调用方 free）
char* dshSettingsGoExport(int sessions, int plugins, const char* dirsJSON, const char* destDir);
// 解析导入压缩包（阻塞）：返回 JSON C 字符串（调用方 free）
char* dshSettingsGoInspect(const char* zipPath);
// 统计冲突项数（阻塞）：-1=出错，>=0=冲突数
int dshSettingsGoCountConflicts(const char* kind, const char* zipPath);
// 恢复子包（阻塞）：返回 JSON C 字符串（调用方 free）
char* dshSettingsGoRestore(const char* kind, const char* zipPath, const char* destDir, int overwrite);

@interface DSHSetController : NSObject <NSTableViewDataSource, NSTableViewDelegate>
@property (nonatomic, strong) NSWindow *window;
@property (nonatomic, strong) NSTableView *catTable;
@property (nonatomic, strong) NSView *generalPane;
@property (nonatomic, strong) NSView *aboutPane;
@property (nonatomic, strong) NSView *logPane;
@property (nonatomic, strong) NSView *exportPane;
@property (nonatomic, strong) NSView *importPane;
@property (nonatomic, strong) NSButton *autoSwitch;
@property (nonatomic, strong) NSTextView *logTV;
@property (nonatomic, strong) NSTextField *logPathLabel;
@property (nonatomic, strong) NSPopUpButton *logPopup;
@property (nonatomic, strong) NSTimer *logTimer;
@property (nonatomic, copy) NSString *lastLogContent;

// 导出页
@property (nonatomic, strong) NSButton *expSessions;
@property (nonatomic, strong) NSButton *expPlugins;
@property (nonatomic, strong) NSButton *expFiles;
@property (nonatomic, strong) NSButton *expBtn;
@property (nonatomic, strong) NSTextField *expStatus;
@property (nonatomic, strong) NSTextView *expDirsView;
@property (nonatomic, strong) NSMutableArray<NSString *> *expDirs;

// 导入页
@property (nonatomic, strong) NSTextField *impPathLabel;
@property (nonatomic, strong) NSTextField *impStatusLabel;
@property (nonatomic, copy) NSString *impZipPath;
@property (nonatomic, strong) NSMutableDictionary<NSString *, NSArray *> *impRows; // kind → @[label, btn]
@property (nonatomic, strong) NSMutableDictionary<NSString *, NSNumber *> *impSizes; // kind → size
@end

static DSHSetController *g_ctrl = nil;

@implementation DSHSetController

// 优先用 Google Noto Sans SC（中英文统一、现代）；系统未安装则回退系统字体。
- (NSFont *)uiFontOfSize:(CGFloat)size weight:(NSFontWeight)w {
    NSFont *f = [NSFont fontWithName:@"Noto Sans SC" size:size];
    if (f) return f;
    return [NSFont systemFontOfSize:size weight:w];
}

- (void)addLabel:(NSString *)text font:(NSFont *)font color:(NSColor *)color frame:(NSRect)frame to:(NSView *)parent {
    NSTextField *l = [NSTextField labelWithString:text];
    l.font = font;
    if (color != nil) l.textColor = color;
    l.frame = frame;
    [parent addSubview:l];
}

- (void)addSeparator:(NSRect)frame to:(NSView *)parent {
    NSBox *sep = [[NSBox alloc] initWithFrame:frame];
    sep.boxType = NSBoxSeparator;
    [parent addSubview:sep];
}

// makeCard 浅灰圆角卡片（1px 边框 + 浅灰底），用于区块分组与日志内容容器。
- (NSBox *)makeCard:(NSRect)frame {
    NSBox *box = [[NSBox alloc] initWithFrame:frame];
    box.boxType = NSBoxCustom;
    box.cornerRadius = 10;
    box.borderWidth = 1;
    box.borderColor = [NSColor colorWithCalibratedRed:0.894 green:0.906 blue:0.925 alpha:1.0]; // #E4E7EC
    box.fillColor = [NSColor colorWithCalibratedRed:0.961 green:0.969 blue:0.980 alpha:1.0];   // #F5F7FA
    box.titlePosition = NSNoTitle;
    if (@available(macOS 10.13, *)) {
        box.contentViewMargins = NSZeroSize;
    }
    return box;
}

- (void)selectPane:(NSInteger)idx {
    [self.generalPane removeFromSuperview];
    [self.aboutPane removeFromSuperview];
    [self.logPane removeFromSuperview];
    [self.exportPane removeFromSuperview];
    [self.importPane removeFromSuperview];
    NSView *pane;
    switch (idx) {
        case 1: pane = self.aboutPane; break;
        case 2: pane = self.logPane; break;
        case 3: pane = self.exportPane; break;
        case 4: pane = self.importPane; break;
        default: pane = self.generalPane; break;
    }
    [self.window.contentView addSubview:pane];
    if (idx == 2) {
        if (!self.logTimer) {
            self.logTimer = [NSTimer scheduledTimerWithTimeInterval:2.0 target:self selector:@selector(refreshLogTick:) userInfo:nil repeats:YES];
        }
        [self refreshLog:YES]; // 打开日志页：滚动到底部一次
    } else {
        [self.logTimer invalidate];
        self.logTimer = nil;
    }
}

- (void)refreshLogTick:(NSTimer *)t {
    if (self.window == nil || !self.window.isVisible) { [self.logTimer invalidate]; self.logTimer = nil; return; }
    [self refreshLog:NO]; // 定时跟随：仅新写入且贴底时滚动
}

- (void)refreshLogPathLabel {
    int which = (self.logPopup == nil) ? 0 : (int)self.logPopup.indexOfSelectedItem;
    if (which < 0) which = 0;
    char *s = dshSettingsGoLogPath(which);
    if (!s) return;
    self.logPathLabel.stringValue = [NSString stringWithUTF8String:s];
    self.logPathLabel.toolTip = [NSString stringWithUTF8String:s];
    free(s);
}

- (NSInteger)numberOfRowsInTableView:(NSTableView *)tv {
    return 5;
}

- (NSView *)tableView:(NSTableView *)tv viewForTableColumn:(NSTableColumn *)col row:(NSInteger)row {
    static NSString *const cid = @"cat-cell";
    NSTableCellView *cell = [tv makeViewWithIdentifier:cid owner:self];
    if (cell == nil) {
        cell = [[NSTableCellView alloc] initWithFrame:NSMakeRect(0, 0, tv.frame.size.width, 26)];
        cell.identifier = cid;
        NSTextField *tf = [NSTextField labelWithString:@""];
        tf.frame = NSMakeRect(10, 3, 130, 20);
        tf.font = [self uiFontOfSize:16 weight:NSFontWeightRegular];
        cell.textField = tf;
        [cell addSubview:tf];
    }
    NSArray *names = @[@"常规", @"关于", @"日志", @"导出", @"导入"];
    cell.textField.stringValue = names[row];
    return cell;
}

- (void)tableViewSelectionDidChange:(NSNotification *)note {
    NSInteger row = [self.catTable selectedRow];
    if (row >= 0) [self selectPane:row];
}

- (void)autostartChanged:(id)sender {
    dshSettingsGoAutostartToggled((int)((NSButton *)sender).state);
}

- (void)checkUpdateClicked:(id)sender {
    dshSettingsGoCheckUpdate();
}

- (void)restartServiceClicked:(id)sender {
    dshSettingsGoRestartService();
}

- (void)refreshLog:(BOOL)forceBottom {
    [self refreshLogPathLabel];
    int which = (self.logPopup == nil) ? 0 : (int)self.logPopup.indexOfSelectedItem;
    if (which < 0) which = 0;
    char *s = dshSettingsGoLoadLog(which);
    if (!s) return;
    NSString *str = [NSString stringWithUTF8String:s];
    free(s);
    // 打开/切换到日志页（或切换文件/清空）：无论内容是否变化都滚动到底部一次
    if (forceBottom) {
        if (![str isEqualToString:self.lastLogContent]) {
            self.logTV.string = str;
            self.lastLogContent = str;
        }
        [self.logTV scrollRangeToVisible:NSMakeRange(str.length, 0)];
        return;
    }
    // 定时跟随：无新写入则不重置，避免把用户手动上翻的位置拉回顶部
    if ([str isEqualToString:self.lastLogContent]) return;
    self.lastLogContent = str;

    NSScrollView *sv = (NSScrollView *)self.logTV.enclosingScrollView;
    BOOL atBottom = YES;
    CGFloat prevY = 0;
    if (sv) {
        CGFloat docH = sv.documentView.frame.size.height;
        CGFloat visH = sv.contentView.bounds.size.height;
        atBottom = (sv.contentView.bounds.origin.y + visH) >= (docH - 2);
        prevY = sv.contentView.bounds.origin.y;
    }
    self.logTV.string = str;
    if (atBottom) {
        // 贴底：跟随追加内容自动滚动到底部
        [self.logTV scrollRangeToVisible:NSMakeRange(str.length, 0)];
    } else if (sv) {
        // 手动上翻：恢复原滚动位置，不打断阅读
        [sv.contentView scrollToPoint:NSMakePoint(0, prevY)];
        [sv reflectScrolledClipView:sv.contentView];
    }
}

- (void)refreshLogClicked:(id)sender { [self refreshLog:YES]; }
- (void)logChanged:(id)sender { [self refreshLog:YES]; }
- (void)clearLogClicked:(id)sender {
    int which = (self.logPopup == nil) ? 0 : (int)self.logPopup.indexOfSelectedItem;
    if (which < 0) which = 0;
    dshSettingsGoClearLog(which);
    [self refreshLog:YES];
}

// ==================== 导出 ====================

- (void)addDirClicked:(id)sender {
    NSOpenPanel *p = [NSOpenPanel openPanel];
    p.canChooseDirectories = YES;
    p.canChooseFiles = NO;
    p.allowsMultipleSelection = NO;
    p.prompt = @"选择";
    if ([p runModal] != NSModalResponseOK) return;
    NSString *dir = p.URL.path;
    if (dir && ![self.expDirs containsObject:dir]) {
        [self.expDirs addObject:dir];
    }
    self.expDirsView.string = [self.expDirs componentsJoinedByString:@"\n"];
}

- (void)exportClicked:(id)sender {
    BOOL sess = (self.expSessions.state == NSControlStateValueOn);
    BOOL plug = (self.expPlugins.state == NSControlStateValueOn);
    BOOL files = (self.expFiles.state == NSControlStateValueOn);
    if (!sess && !plug && !files) {
        NSAlert *a = [[NSAlert alloc] init];
        a.messageText = @"请至少勾选一项导出内容。";
        [a runModal];
        return;
    }
    if (files && self.expDirs.count == 0) {
        NSAlert *a = [[NSAlert alloc] init];
        a.messageText = @"已勾选「需要打包的文件目录」，请先点击「选择目录…」添加目录。";
        [a runModal];
        return;
    }
    NSOpenPanel *p = [NSOpenPanel openPanel];
    p.canChooseDirectories = YES;
    p.canChooseFiles = NO;
    p.allowsMultipleSelection = NO;
    p.prompt = @"选择保存位置";
    if ([p runModal] != NSModalResponseOK) return;
    NSString *dest = p.URL.path;

    self.expStatus.stringValue = @"正在导出…";
    self.expBtn.enabled = NO;
    NSData *dirsJSON = [NSJSONSerialization dataWithJSONObject:self.expDirs options:0 error:nil];
    NSString *dirsStr = dirsJSON ? [[NSString alloc] initWithData:dirsJSON encoding:NSUTF8StringEncoding] : @"[]";
    dispatch_async(dispatch_get_global_queue(DISPATCH_QUEUE_PRIORITY_DEFAULT, 0), ^{
        char *res = dshSettingsGoExport(sess ? 1 : 0, plug ? 1 : 0, dirsStr.UTF8String, dest.UTF8String);
        NSString *json = res ? [NSString stringWithUTF8String:res] : @"";
        if (res) free(res);
        dispatch_async(dispatch_get_main_queue(), ^{
            self.expBtn.enabled = YES;
            NSDictionary *d = [self parseJSON:json];
            if (d && [d[@"ok"] boolValue]) {
                self.expStatus.stringValue = @"导出完成";
                NSAlert *a = [[NSAlert alloc] init];
                a.messageText = @"导出完成";
                a.informativeText = d[@"path"] ?: @"";
                [a runModal];
            } else {
                self.expStatus.stringValue = @"导出失败";
                NSAlert *a = [[NSAlert alloc] init];
                a.messageText = @"导出失败";
                a.informativeText = d[@"error"] ?: @"未知错误";
                [a runModal];
            }
        });
    });
}

// ==================== 导入 ====================

- (NSDictionary *)parseJSON:(NSString *)json {
    if (!json || json.length == 0) return nil;
    NSData *data = [json dataUsingEncoding:NSUTF8StringEncoding];
    NSError *err = nil;
    id obj = [NSJSONSerialization JSONObjectWithData:data options:0 error:&err];
    if (err || ![obj isKindOfClass:[NSDictionary class]]) return nil;
    return (NSDictionary *)obj;
}

- (void)hideImportRows {
    for (NSArray *pair in self.impRows.allValues) {
        [pair[0] setHidden:YES];
        [pair[1] setHidden:YES];
    }
    [self.impSizes removeAllObjects];
}

- (void)addImportClicked:(id)sender {
    NSOpenPanel *p = [NSOpenPanel openPanel];
    p.canChooseFiles = YES;
    p.canChooseDirectories = NO;
    p.allowsMultipleSelection = NO;
    p.prompt = @"选择";
    if (@available(macOS 11.0, *)) {
        p.allowedContentTypes = @[[UTType typeWithFilenameExtension:@"zip"]];
    } else {
        p.allowedFileTypes = @[@"zip"];
    }
    if ([p runModal] != NSModalResponseOK) return;
    NSString *path = p.URL.path;
    self.impZipPath = path;
    self.impPathLabel.stringValue = path;
    self.impStatusLabel.stringValue = @"正在解析压缩包…";
    [self hideImportRows];
    dispatch_async(dispatch_get_global_queue(DISPATCH_QUEUE_PRIORITY_DEFAULT, 0), ^{
        char *res = dshSettingsGoInspect(path.UTF8String);
        NSString *json = res ? [NSString stringWithUTF8String:res] : @"";
        if (res) free(res);
        dispatch_async(dispatch_get_main_queue(), ^{
            [self handleInspectResult:json];
        });
    });
}

- (void)handleInspectResult:(NSString *)json {
    NSDictionary *d = [self parseJSON:json];
    if (!d || ![d[@"ok"] boolValue]) {
        self.impStatusLabel.stringValue = [NSString stringWithFormat:@"解析异常：%@", (d[@"error"] ?: @"无法解析该压缩包")];
        return;
    }
    NSArray *items = d[@"items"];
    if (![items isKindOfClass:[NSArray class]] || items.count == 0) {
        self.impStatusLabel.stringValue = @"解析异常：压缩包中没有可恢复的内容";
        return;
    }
    for (NSDictionary *it in items) {
        NSString *kind = it[@"kind"];
        NSArray *pair = self.impRows[kind];
        if (!pair) continue;
        NSNumber *size = it[@"size"];
        NSString *label = it[@"label"] ?: kind;
        if (size && size.longLongValue > 0) {
            label = [NSString stringWithFormat:@"%@（%.1f MB）", label, size.longLongValue / 1024.0 / 1024.0];
        }
        ((NSTextField *)pair[0]).stringValue = label;
        [pair[0] setHidden:NO];
        [pair[1] setHidden:NO];
        if (size) self.impSizes[kind] = size;
    }
    self.impStatusLabel.stringValue = [NSString stringWithFormat:@"解析成功：共 %lu 个可恢复项，点击右侧「恢复」逐项恢复。", (unsigned long)items.count];
}

- (void)restoreClicked:(NSButton *)btn {
    NSString *kind = nil;
    if (btn.tag == 1) kind = @"sessions";
    else if (btn.tag == 2) kind = @"plugins";
    else if (btn.tag == 3) kind = @"files";
    if (!kind || !self.impZipPath) return;
    if ([kind isEqualToString:@"files"]) {
        NSOpenPanel *p = [NSOpenPanel openPanel];
        p.canChooseDirectories = YES;
        p.canChooseFiles = NO;
        p.allowsMultipleSelection = NO;
        p.prompt = @"选择解压位置";
        if ([p runModal] != NSModalResponseOK) return;
        [self runRestore:kind destDir:p.URL.path overwrite:1];
        return;
    }
    // 会话/插件：先查冲突（后台），有冲突弹窗询问
    NSString *path = self.impZipPath;
    const char *k = kind.UTF8String;
    dispatch_async(dispatch_get_global_queue(DISPATCH_QUEUE_PRIORITY_DEFAULT, 0), ^{
        int n = dshSettingsGoCountConflicts(k, path.UTF8String);
        dispatch_async(dispatch_get_main_queue(), ^{
            if (n < 0) {
                self.impStatusLabel.stringValue = @"恢复失败：无法检查冲突。";
                return;
            }
            if (n == 0) {
                [self runRestore:kind destDir:@"" overwrite:1];
                return;
            }
            NSAlert *a = [[NSAlert alloc] init];
            a.messageText = [NSString stringWithFormat:@"检测到 %d 项与当前环境存在冲突。", n];
            a.informativeText = @"覆盖更新：恢复前备份现有数据，恢复成功后自动清理备份。\n跳过已有：保留现有数据，仅补充缺失内容。";
            [a addButtonWithTitle:@"覆盖更新"];
            [a addButtonWithTitle:@"跳过已有"];
            [a addButtonWithTitle:@"取消"];
            NSModalResponse r = [a runModal];
            if (r == NSAlertFirstButtonReturn) {
                [self runRestore:kind destDir:@"" overwrite:1];
            } else if (r == NSAlertSecondButtonReturn) {
                [self runRestore:kind destDir:@"" overwrite:0];
            } else {
                self.impStatusLabel.stringValue = @"已取消恢复。";
            }
        });
    });
}

- (void)runRestore:(NSString *)kind destDir:(NSString *)dest overwrite:(int)overwrite {
    self.impStatusLabel.stringValue = @"正在恢复…";
    [self setImportButtonsEnabled:NO];
    NSString *path = self.impZipPath;
    dispatch_async(dispatch_get_global_queue(DISPATCH_QUEUE_PRIORITY_DEFAULT, 0), ^{
        char *res = dshSettingsGoRestore(kind.UTF8String, path.UTF8String, dest.UTF8String, overwrite);
        NSString *json = res ? [NSString stringWithUTF8String:res] : @"";
        if (res) free(res);
        dispatch_async(dispatch_get_main_queue(), ^{
            [self setImportButtonsEnabled:YES];
            NSDictionary *d = [self parseJSON:json];
            if (d && [d[@"ok"] boolValue]) {
                self.impStatusLabel.stringValue = d[@"message"] ?: @"恢复完成";
                NSAlert *a = [[NSAlert alloc] init];
                a.messageText = @"恢复完成";
                a.informativeText = d[@"message"] ?: @"";
                [a runModal];
            } else {
                self.impStatusLabel.stringValue = @"恢复失败";
                NSAlert *a = [[NSAlert alloc] init];
                a.messageText = @"恢复失败";
                a.informativeText = d[@"error"] ?: @"未知错误";
                [a runModal];
            }
        });
    });
}

- (void)setImportButtonsEnabled:(BOOL)enabled {
    for (NSArray *pair in self.impRows.allValues) {
        [(NSButton *)pair[1] setEnabled:enabled];
    }
}

// ==================== 窗口构建 ====================

- (void)buildWindowWithVersion:(const char *)ver harness:(const char *)hver autostart:(int)autoOn {
    NSRect contentRect = NSMakeRect(0, 0, 600, 420);
    self.window = [[NSWindow alloc] initWithContentRect:contentRect
                                              styleMask:(NSWindowStyleMaskTitled | NSWindowStyleMaskClosable)
                                                backing:NSBackingStoreBuffered
                                                  defer:NO];
    self.window.title = @"设置";
    [self.window center];
    [self.window setReleasedWhenClosed:NO];

    NSView *root = [[NSView alloc] initWithFrame:contentRect];

    // 左侧分类列表
    self.catTable = [[NSTableView alloc] initWithFrame:NSMakeRect(0, 0, 150, 420)];
    NSTableColumn *col = [[NSTableColumn alloc] initWithIdentifier:@"cat"];
    [col setWidth:150];
    [self.catTable addTableColumn:col];
    self.catTable.headerView = nil;
    self.catTable.dataSource = self;
    self.catTable.delegate = self;
    self.catTable.selectionHighlightStyle = NSTableViewSelectionHighlightStyleSourceList;
    self.catTable.backgroundColor = [NSColor colorWithCalibratedRed:0.961 green:0.969 blue:0.980 alpha:1.0];
    [self.catTable reloadData];

    NSScrollView *sv = [[NSScrollView alloc] initWithFrame:NSMakeRect(0, 0, 150, 420)];
    sv.documentView = self.catTable;
    sv.hasVerticalScroller = NO;
    sv.drawsBackground = YES;
    sv.autohidesScrollers = YES;
    [root addSubview:sv];

    // 常规面板：开机自启动开关 + 后台服务（浅灰圆角卡片分组，替代分割线）
    self.generalPane = [[NSView alloc] initWithFrame:NSMakeRect(160, 0, 440, 420)];
    [self addLabel:@"常规" font:[self uiFontOfSize:19 weight:NSFontWeightSemibold] color:nil frame:NSMakeRect(0, 372, 300, 24) to:self.generalPane];

    // 区块卡片 1：开机自启动（先添加以垫底）
    [self.generalPane addSubview:[self makeCard:NSMakeRect(0, 296, 430, 74)]];
    [self addLabel:@"开机自启动" font:[self uiFontOfSize:17 weight:NSFontWeightRegular] color:nil frame:NSMakeRect(14, 320, 220, 24) to:self.generalPane];

    self.autoSwitch = [[NSButton alloc] initWithFrame:NSMakeRect(310, 318, 60, 24)];
    [self.autoSwitch setButtonType:NSButtonTypeSwitch];
    self.autoSwitch.title = @"";
    self.autoSwitch.state = (autoOn ? NSControlStateValueOn : NSControlStateValueOff);
    self.autoSwitch.target = self;
    self.autoSwitch.action = @selector(autostartChanged:);
    [self.generalPane addSubview:self.autoSwitch];

    // 区块卡片 2：后台服务（先添加以垫底）
    [self.generalPane addSubview:[self makeCard:NSMakeRect(0, 194, 430, 86)]];

    // 常规面板：后台服务状态（绿/红圆点）+ 重启按钮
    char *svcState = dshSettingsGoServiceState();
    BOOL svcRunning = (strcmp(svcState, "运行中") == 0);
    free(svcState);
    NSString *svcText = [NSString stringWithFormat:@"后台服务：%@", svcRunning ? @"运行中" : @"已停止"];
    NSMutableAttributedString *svcAttr = [[NSMutableAttributedString alloc] initWithString:[NSString stringWithFormat:@"● %@", svcText]];
    NSColor *svcDot = svcRunning ? [NSColor systemGreenColor] : [NSColor systemRedColor];
    [svcAttr addAttribute:NSForegroundColorAttributeName value:svcDot range:NSMakeRange(0, 1)];
    [svcAttr addAttribute:NSForegroundColorAttributeName value:[NSColor secondaryLabelColor] range:NSMakeRange(2, svcAttr.length - 2)];
    NSTextField *svcLabel = [NSTextField labelWithAttributedString:svcAttr];
    svcLabel.font = [self uiFontOfSize:16 weight:NSFontWeightRegular];
    svcLabel.frame = NSMakeRect(14, 240, 320, 18);
    [self.generalPane addSubview:svcLabel];

    NSButton *restartBtn = [[NSButton alloc] initWithFrame:NSMakeRect(14, 206, 108, 30)];
    restartBtn.title = @"重启后台服务";
    restartBtn.bezelStyle = NSBezelStyleRounded;
    restartBtn.font = [self uiFontOfSize:13 weight:NSFontWeightSemibold];
    restartBtn.target = self;
    restartBtn.action = @selector(restartServiceClicked:);
    [self.generalPane addSubview:restartBtn];

    // 关于面板：dsh-systray 版本号 + DeepSeek Harness 版本号 + 检查更新
    self.aboutPane = [[NSView alloc] initWithFrame:NSMakeRect(160, 0, 440, 420)];
    [self addLabel:@"关于" font:[self uiFontOfSize:19 weight:NSFontWeightSemibold] color:nil frame:NSMakeRect(0, 372, 300, 24) to:self.aboutPane];
    [self addLabel:@"dsh-systray 版本号" font:[self uiFontOfSize:17 weight:NSFontWeightRegular] color:nil frame:NSMakeRect(0, 326, 220, 24) to:self.aboutPane];
    [self addLabel:[NSString stringWithFormat:@"%s", ver]
               font:[self uiFontOfSize:19 weight:NSFontWeightSemibold]
              color:[NSColor colorWithCalibratedRed:0.114 green:0.306 blue:0.847 alpha:1.0]
              frame:NSMakeRect(0, 300, 220, 26)
                   to:self.aboutPane];
    [self addLabel:@"DeepSeek Harness 版本号" font:[self uiFontOfSize:17 weight:NSFontWeightRegular] color:nil frame:NSMakeRect(0, 270, 240, 22) to:self.aboutPane];
    [self addLabel:[NSString stringWithFormat:@"%s", hver]
               font:[self uiFontOfSize:19 weight:NSFontWeightSemibold]
              color:[NSColor colorWithCalibratedRed:0.114 green:0.306 blue:0.847 alpha:1.0]
              frame:NSMakeRect(0, 246, 220, 26)
                   to:self.aboutPane];

    NSButton *checkBtn = [[NSButton alloc] initWithFrame:NSMakeRect(0, 202, 108, 30)];
    checkBtn.title = @"检查更新";
    checkBtn.bezelStyle = NSBezelStyleRounded;
    checkBtn.font = [self uiFontOfSize:13 weight:NSFontWeightSemibold];
    checkBtn.target = self;
    checkBtn.action = @selector(checkUpdateClicked:);
    [self.aboutPane addSubview:checkBtn];

    // 日志面板：完整路径说明 + 圆角下拉选择 + 只读日志（浅灰圆角卡片，14px 等宽）
    self.logPane = [[NSView alloc] initWithFrame:NSMakeRect(160, 0, 440, 420)];
    self.logPathLabel = [NSTextField labelWithString:@""];
    self.logPathLabel.font = [self uiFontOfSize:12 weight:NSFontWeightRegular];
    self.logPathLabel.textColor = [NSColor secondaryLabelColor];
    self.logPathLabel.frame = NSMakeRect(0, 378, 430, 18);
    self.logPathLabel.lineBreakMode = NSLineBreakByTruncatingMiddle;
    [self.logPane addSubview:self.logPathLabel];

    self.logPopup = [[NSPopUpButton alloc] initWithFrame:NSMakeRect(0, 340, 130, 30)];
    self.logPopup.bezelStyle = NSBezelStyleRounded;
    self.logPopup.font = [self uiFontOfSize:15 weight:NSFontWeightRegular];
    [self.logPopup addItemsWithTitles:@[@"app.log", @"server.log"]];
    self.logPopup.target = self;
    self.logPopup.action = @selector(logChanged:);
    [self.logPane addSubview:self.logPopup];

    NSButton *refresh = [[NSButton alloc] initWithFrame:NSMakeRect(140, 336, 90, 30)];
    refresh.title = @"清空";
    refresh.bezelStyle = NSBezelStyleRounded;
    refresh.font = [self uiFontOfSize:13 weight:NSFontWeightSemibold];
    refresh.target = self;
    refresh.action = @selector(clearLogClicked:);
    if (@available(macOS 10.14, *)) {
        refresh.contentTintColor = [NSColor colorWithCalibratedRed:0.114 green:0.306 blue:0.847 alpha:1.0];
    }
    [self.logPane addSubview:refresh];

    // 日志内容卡片（浅灰圆角）+ 无边框滚动区（先添加卡片以垫底）
    [self.logPane addSubview:[self makeCard:NSMakeRect(0, 8, 432, 322)]];

    NSScrollView *logSV = [[NSScrollView alloc] initWithFrame:NSMakeRect(5, 13, 422, 312)];
    logSV.borderType = NSNoBorder;
    logSV.drawsBackground = NO;
    logSV.hasVerticalScroller = YES;
    logSV.autohidesScrollers = YES;
    logSV.scrollerStyle = NSScrollerStyleOverlay;
    NSTextView *tv = [[NSTextView alloc] initWithFrame:NSMakeRect(0, 0, 422, 312)];
    tv.editable = NO;
    tv.selectable = YES;
    tv.drawsBackground = YES;
    tv.backgroundColor = [NSColor colorWithCalibratedRed:0.961 green:0.969 blue:0.980 alpha:1.0]; // #F5F7FA 与卡片同色
    tv.font = [NSFont monospacedSystemFontOfSize:14 weight:NSFontWeightRegular]; // 日志字体 14px
    tv.textContainerInset = NSMakeSize(10, 8);
    logSV.documentView = tv;
    [self.logPane addSubview:logSV];
    self.logTV = tv;
    [self refreshLogPathLabel];

    // 导出面板
    self.exportPane = [[NSView alloc] initWithFrame:NSMakeRect(160, 0, 440, 420)];
    [self addLabel:@"导出" font:[self uiFontOfSize:19 weight:NSFontWeightSemibold] color:nil frame:NSMakeRect(0, 372, 300, 24) to:self.exportPane];
    self.expSessions = [NSButton checkboxWithTitle:@"所有历史会话" target:nil action:nil];
    self.expSessions.frame = NSMakeRect(0, 334, 280, 24);
    self.expSessions.font = [self uiFontOfSize:16 weight:NSFontWeightRegular];
    [self.exportPane addSubview:self.expSessions];
    [self addLabel:@"sessions.zip · ~/.dsh/sessions" font:[self uiFontOfSize:12 weight:NSFontWeightRegular] color:[NSColor secondaryLabelColor] frame:NSMakeRect(22, 316, 400, 16) to:self.exportPane];

    self.expPlugins = [NSButton checkboxWithTitle:@"已安装的插件" target:nil action:nil];
    self.expPlugins.frame = NSMakeRect(0, 288, 280, 24);
    self.expPlugins.font = [self uiFontOfSize:16 weight:NSFontWeightRegular];
    [self.exportPane addSubview:self.expPlugins];
    [self addLabel:@"plugins.zip · ~/.dsh/profiles/node_modules" font:[self uiFontOfSize:12 weight:NSFontWeightRegular] color:[NSColor secondaryLabelColor] frame:NSMakeRect(22, 270, 400, 16) to:self.exportPane];

    self.expFiles = [NSButton checkboxWithTitle:@"需要打包的文件目录" target:nil action:nil];
    self.expFiles.frame = NSMakeRect(0, 242, 280, 24);
    self.expFiles.font = [self uiFontOfSize:16 weight:NSFontWeightRegular];
    [self.exportPane addSubview:self.expFiles];
    [self addLabel:@"files.zip · 恢复时选择解压位置" font:[self uiFontOfSize:12 weight:NSFontWeightRegular] color:[NSColor secondaryLabelColor] frame:NSMakeRect(22, 224, 400, 16) to:self.exportPane];

    NSButton *addDirBtn = [[NSButton alloc] initWithFrame:NSMakeRect(0, 186, 110, 28)];
    addDirBtn.title = @"选择目录…";
    addDirBtn.bezelStyle = NSBezelStyleRounded;
    addDirBtn.font = [self uiFontOfSize:13 weight:NSFontWeightSemibold];
    addDirBtn.target = self;
    addDirBtn.action = @selector(addDirClicked:);
    [self.exportPane addSubview:addDirBtn];

    self.expDirs = [NSMutableArray array];
    self.expDirsView = [[NSTextView alloc] initWithFrame:NSMakeRect(0, 0, 430, 64)];
    self.expDirsView.editable = NO;
    self.expDirsView.font = [self uiFontOfSize:12 weight:NSFontWeightRegular];
    NSScrollView *dirsSV = [[NSScrollView alloc] initWithFrame:NSMakeRect(0, 112, 430, 66)];
    dirsSV.documentView = self.expDirsView;
    dirsSV.hasVerticalScroller = YES;
    dirsSV.autohidesScrollers = YES;
    [self.exportPane addSubview:dirsSV];

    self.expBtn = [[NSButton alloc] initWithFrame:NSMakeRect(0, 68, 120, 32)];
    self.expBtn.title = @"导出…";
    self.expBtn.bezelStyle = NSBezelStyleRounded;
    self.expBtn.font = [self uiFontOfSize:14 weight:NSFontWeightSemibold];
    self.expBtn.target = self;
    self.expBtn.action = @selector(exportClicked:);
    [self.exportPane addSubview:self.expBtn];

    self.expStatus = [NSTextField labelWithString:@""];
    self.expStatus.font = [self uiFontOfSize:12 weight:NSFontWeightRegular];
    self.expStatus.textColor = [NSColor secondaryLabelColor];
    self.expStatus.frame = NSMakeRect(0, 36, 430, 18);
    [self.exportPane addSubview:self.expStatus];

    // 导入面板
    self.importPane = [[NSView alloc] initWithFrame:NSMakeRect(160, 0, 440, 420)];
    [self addLabel:@"导入" font:[self uiFontOfSize:19 weight:NSFontWeightSemibold] color:nil frame:NSMakeRect(0, 372, 300, 24) to:self.importPane];

    NSButton *addImportBtn = [[NSButton alloc] initWithFrame:NSMakeRect(0, 328, 180, 30)];
    addImportBtn.title = @"添加导入压缩包…";
    addImportBtn.bezelStyle = NSBezelStyleRounded;
    addImportBtn.font = [self uiFontOfSize:13 weight:NSFontWeightSemibold];
    addImportBtn.target = self;
    addImportBtn.action = @selector(addImportClicked:);
    [self.importPane addSubview:addImportBtn];

    self.impPathLabel = [NSTextField labelWithString:@"（尚未选择导入压缩包）"];
    self.impPathLabel.font = [self uiFontOfSize:12 weight:NSFontWeightRegular];
    self.impPathLabel.textColor = [NSColor secondaryLabelColor];
    self.impPathLabel.frame = NSMakeRect(0, 300, 430, 20);
    self.impPathLabel.lineBreakMode = NSLineBreakByTruncatingMiddle;
    [self.importPane addSubview:self.impPathLabel];

    self.impStatusLabel = [NSTextField labelWithString:@"点击上方按钮选择 dsh-systray-export 压缩包。"];
    self.impStatusLabel.font = [self uiFontOfSize:12 weight:NSFontWeightRegular];
    self.impStatusLabel.textColor = [NSColor secondaryLabelColor];
    self.impStatusLabel.frame = NSMakeRect(0, 272, 430, 22);
    self.impStatusLabel.lineBreakMode = NSLineBreakByTruncatingTail;
    [self.importPane addSubview:self.impStatusLabel];

    self.impRows = [NSMutableDictionary dictionary];
    self.impSizes = [NSMutableDictionary dictionary];
    NSArray *kinds = @[@"sessions", @"plugins", @"files"];
    NSInteger tags = 1;
    CGFloat y = 228;
    for (NSString *kind in kinds) {
        NSTextField *rowLabel = [NSTextField labelWithString:@""];
        rowLabel.font = [self uiFontOfSize:16 weight:NSFontWeightRegular];
        rowLabel.frame = NSMakeRect(0, y + 3, 320, 24);
        rowLabel.hidden = YES;
        [self.importPane addSubview:rowLabel];

        NSButton *restoreBtn = [[NSButton alloc] initWithFrame:NSMakeRect(330, y, 96, 28)];
        restoreBtn.title = @"恢复";
        restoreBtn.bezelStyle = NSBezelStyleRounded;
        restoreBtn.font = [self uiFontOfSize:13 weight:NSFontWeightSemibold];
        restoreBtn.tag = tags++;
        restoreBtn.target = self;
        restoreBtn.action = @selector(restoreClicked:);
        restoreBtn.hidden = YES;
        [self.importPane addSubview:restoreBtn];

        self.impRows[kind] = @[rowLabel, restoreBtn];
        y -= 44;
    }

    self.window.contentView = root;
    [self selectPane:0];
    [self.catTable selectRowIndexes:[NSIndexSet indexSetWithIndex:0] byExtendingSelection:NO];
}

- (void)showWithVersion:(const char *)ver harness:(const char *)hver autostart:(int)autoOn {
    if (self.window == nil) {
        [self buildWindowWithVersion:ver harness:hver autostart:autoOn];
    }
    self.autoSwitch.state = (autoOn ? NSControlStateValueOn : NSControlStateValueOff);
    [self.window makeKeyAndOrderFront:nil];
    [NSApp activateIgnoringOtherApps:YES];
}

@end

// dsh_settings_open 在 AppKit 主线程创建/前置设置窗口（Go 侧调用，可来自任意 goroutine）。
// version/harnessVersion 为 Go 侧 C 字符串，调用返回后即被释放，因此此处同步 strdup 拷贝一份供异步块使用。
void dsh_settings_open(const char *version, const char *harnessVersion, int autostartOn) {
    char *verCopy = strdup(version);
    char *hverCopy = strdup(harnessVersion);
    dispatch_async(dispatch_get_main_queue(), ^{
        if (g_ctrl == nil) {
            g_ctrl = [[DSHSetController alloc] init];
        }
        [g_ctrl showWithVersion:verCopy harness:hverCopy autostart:autostartOn];
        free(verCopy);
        free(hverCopy);
    });
}
