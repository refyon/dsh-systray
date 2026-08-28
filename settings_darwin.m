// 设置窗口原生实现（macOS Cocoa）：左侧分类列表（常规 / 关于）+ 右侧内容面板。
// 由 cgo 编译（settings_darwin.go 中 #cgo darwin CFLAGS: -x objective-c -fobjc-arc）。
#import <Cocoa/Cocoa.h>
#import <dispatch/dispatch.h>
#import <stdlib.h>
#import <string.h>

// Go 侧回调（settings_darwin.go //export）
extern void dshSettingsGoAutostartToggled(int on);
extern void dshSettingsGoCheckUpdate(void);

@interface DSHSetController : NSObject <NSTableViewDataSource, NSTableViewDelegate>
@property (nonatomic, strong) NSWindow *window;
@property (nonatomic, strong) NSTableView *catTable;
@property (nonatomic, strong) NSView *generalPane;
@property (nonatomic, strong) NSView *aboutPane;
@property (nonatomic, strong) NSButton *autoSwitch;
@end

static DSHSetController *g_ctrl = nil;

@implementation DSHSetController

- (void)addLabel:(NSString *)text font:(NSFont *)font color:(NSColor *)color frame:(NSRect)frame to:(NSView *)parent {
    NSTextField *l = [NSTextField labelWithString:text];
    l.font = font;
    if (color != nil) l.textColor = color;
    l.frame = frame;
    [parent addSubview:l];
}

- (void)selectPane:(NSInteger)idx {
    [self.generalPane removeFromSuperview];
    [self.aboutPane removeFromSuperview];
    NSView *pane = (idx == 0) ? self.generalPane : self.aboutPane;
    [self.window.contentView addSubview:pane];
}

- (NSInteger)numberOfRowsInTableView:(NSTableView *)tv {
    return 2;
}

- (NSView *)tableView:(NSTableView *)tv viewForTableColumn:(NSTableColumn *)col row:(NSInteger)row {
    static NSString *const cid = @"cat-cell";
    NSTableCellView *cell = [tv makeViewWithIdentifier:cid owner:self];
    if (cell == nil) {
        cell = [[NSTableCellView alloc] initWithFrame:NSMakeRect(0, 0, tv.frame.size.width, 26)];
        cell.identifier = cid;
        NSTextField *tf = [NSTextField labelWithString:@""];
        tf.frame = NSMakeRect(10, 3, 130, 20);
        tf.font = [NSFont systemFontOfSize:13];
        cell.textField = tf;
        [cell addSubview:tf];
    }
    cell.textField.stringValue = (row == 0) ? @"常规" : @"关于";
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

- (void)buildWindowWithVersion:(const char *)ver autostart:(int)autoOn {
    NSRect contentRect = NSMakeRect(0, 0, 560, 360);
    self.window = [[NSWindow alloc] initWithContentRect:contentRect
                                              styleMask:(NSWindowStyleMaskTitled | NSWindowStyleMaskClosable)
                                                backing:NSBackingStoreBuffered
                                                  defer:NO];
    self.window.title = @"设置";
    [self.window center];
    [self.window setReleasedWhenClosed:NO];

    NSView *root = [[NSView alloc] initWithFrame:contentRect];

    // 左侧分类列表
    self.catTable = [[NSTableView alloc] initWithFrame:NSMakeRect(0, 0, 150, 300)];
    NSTableColumn *col = [[NSTableColumn alloc] initWithIdentifier:@"cat"];
    [col setWidth:150];
    [self.catTable addTableColumn:col];
    self.catTable.headerView = nil;
    self.catTable.dataSource = self;
    self.catTable.delegate = self;
    self.catTable.selectionHighlightStyle = NSTableViewSelectionHighlightStyleSourceList;
    self.catTable.backgroundColor = [NSColor colorWithCalibratedWhite:0.97 alpha:1.0];
    [self.catTable reloadData];

    NSScrollView *sv = [[NSScrollView alloc] initWithFrame:NSMakeRect(0, 0, 150, 360)];
    sv.documentView = self.catTable;
    sv.hasVerticalScroller = NO;
    sv.drawsBackground = YES;
    sv.autohidesScrollers = YES;
    [root addSubview:sv];

    // 常规面板：开机自启动开关
    self.generalPane = [[NSView alloc] initWithFrame:NSMakeRect(160, 0, 400, 360)];
    [self addLabel:@"常规" font:[NSFont systemFontOfSize:16 weight:NSFontWeightSemibold] color:nil frame:NSMakeRect(0, 316, 300, 24) to:self.generalPane];
    [self addLabel:@"开机自启动" font:[NSFont systemFontOfSize:14] color:nil frame:NSMakeRect(0, 270, 200, 22) to:self.generalPane];
    [self addLabel:@"登录系统时自动启动托盘程序" font:[NSFont systemFontOfSize:12] color:[NSColor secondaryLabelColor] frame:NSMakeRect(0, 248, 320, 18) to:self.generalPane];

    self.autoSwitch = [[NSButton alloc] initWithFrame:NSMakeRect(310, 268, 60, 24)];
    [self.autoSwitch setButtonType:NSButtonTypeSwitch];
    self.autoSwitch.title = @"";
    self.autoSwitch.state = (autoOn ? NSControlStateValueOn : NSControlStateValueOff);
    self.autoSwitch.target = self;
    self.autoSwitch.action = @selector(autostartChanged:);
    [self.generalPane addSubview:self.autoSwitch];

    // 关于面板：版本号 + 检查更新
    self.aboutPane = [[NSView alloc] initWithFrame:NSMakeRect(160, 0, 400, 360)];
    [self addLabel:@"关于" font:[NSFont systemFontOfSize:16 weight:NSFontWeightSemibold] color:nil frame:NSMakeRect(0, 316, 300, 24) to:self.aboutPane];
    [self addLabel:@"当前版本号" font:[NSFont systemFontOfSize:14] color:nil frame:NSMakeRect(0, 270, 200, 22) to:self.aboutPane];
    [self addLabel:[NSString stringWithFormat:@"%s", ver]
               font:[NSFont systemFontOfSize:16 weight:NSFontWeightSemibold]
              color:[NSColor colorWithCalibratedRed:0.30 green:0.42 blue:1.0 alpha:1.0]
              frame:NSMakeRect(0, 244, 220, 24)
                  to:self.aboutPane];

    NSButton *checkBtn = [[NSButton alloc] initWithFrame:NSMakeRect(0, 196, 120, 32)];
    checkBtn.title = @"检查更新";
    checkBtn.bezelStyle = NSBezelStyleRounded;
    checkBtn.target = self;
    checkBtn.action = @selector(checkUpdateClicked:);
    [self.aboutPane addSubview:checkBtn];

    self.window.contentView = root;
    [self selectPane:0];
    [self.catTable selectRowIndexes:[NSIndexSet indexSetWithIndex:0] byExtendingSelection:NO];
}

- (void)showWithVersion:(const char *)ver autostart:(int)autoOn {
    if (self.window == nil) {
        [self buildWindowWithVersion:ver autostart:autoOn];
    }
    self.autoSwitch.state = (autoOn ? NSControlStateValueOn : NSControlStateValueOff);
    [self.window makeKeyAndOrderFront:nil];
    [NSApp activateIgnoringOtherApps:YES];
}

@end

// dsh_settings_open 在 AppKit 主线程创建/前置设置窗口（Go 侧调用，可来自任意 goroutine）。
// version 为 Go 侧 C 字符串，调用返回后即被释放，因此此处同步 strdup 拷贝一份供异步块使用。
void dsh_settings_open(const char *version, int autostartOn) {
    char *verCopy = strdup(version);
    dispatch_async(dispatch_get_main_queue(), ^{
        if (g_ctrl == nil) {
            g_ctrl = [[DSHSetController alloc] init];
        }
        [g_ctrl showWithVersion:verCopy autostart:autostartOn];
        free(verCopy);
    });
}
