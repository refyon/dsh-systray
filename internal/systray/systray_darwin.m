#import <Cocoa/Cocoa.h>
#import <dispatch/dispatch.h>
#include "systray.h"

#if __MAC_OS_X_VERSION_MIN_REQUIRED < 101400

    #ifndef NSControlStateValueOff
      #define NSControlStateValueOff NSOffState
    #endif

    #ifndef NSControlStateValueOn
      #define NSControlStateValueOn NSOnState
    #endif

#endif

@interface MenuItem : NSObject {
  @public
    NSNumber* menuId;
    NSNumber* parentMenuId;
    NSString* title;
    NSString* tooltip;
    NSString* shortcutKey;
    short disabled;
    short checked;
}

-(id) initWithId: (int)theMenuId
withParentMenuId: (int)theParentMenuId
       withTitle: (const char*)theTitle
     withTooltip: (const char*)theTooltip
 withShortcutKey: (const char*)theShortcutKey
    withDisabled: (short)theDisabled
     withChecked: (short)theChecked;
     @end
     @implementation MenuItem
     -(id) initWithId: (int)theMenuId
     withParentMenuId: (int)theParentMenuId
            withTitle: (const char*)theTitle
          withTooltip: (const char*)theTooltip
      withShortcutKey: (const char*)theShortcutKey
         withDisabled: (short)theDisabled
          withChecked: (short)theChecked
{
  menuId = [NSNumber numberWithInt:theMenuId];
  parentMenuId = [NSNumber numberWithInt:theParentMenuId];
  title = [[NSString alloc] initWithCString:theTitle
                                   encoding:NSUTF8StringEncoding];
  tooltip = [[NSString alloc] initWithCString:theTooltip
                                     encoding:NSUTF8StringEncoding];
  disabled = theDisabled;
  checked = theChecked;
  return self;
}
@end

@interface SystrayAppDelegate: NSObject <NSApplicationDelegate>
  - (void) add_or_update_menu_item:(MenuItem*) item;
  - (IBAction)menuHandler:(id)sender;
  - (void)statusOnClick:(NSButton *)btn;
  - (void)registerPowerNotifications;
  - (void)systemWillPowerOff:(NSNotification *)note;
  - (void)systemSessionResigned:(NSNotification *)note;
  - (void)systemSessionBecameActive:(NSNotification *)note;
  @property (assign) IBOutlet NSWindow *window;
  @end

@implementation SystrayAppDelegate {
  NSStatusItem *statusItem;
  NSMenu *menu;
  NSCondition* cond;
}

@synthesize window = _window;

- (void)applicationDidFinishLaunching:(NSNotification *)aNotification {
  self->statusItem = [[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
  self->menu = [[NSMenu alloc] init];
  [self->menu setAutoenablesItems: FALSE];
  // 常驻挂接菜单：由 AppKit 原生管理点击弹菜单（左/右键一致），
  // 避免 show_menu 的「临时挂接 + performClick + 立即摘除」技巧在较新 macOS 上失效
  // （表现为点击图标无菜单弹出）。挂接后 statusItem.button 的自定义 action 不再生效，
  // 菜单项增删改走同一 NSMenu 对象即可。
  [self->statusItem setMenu:self->menu];
  [self registerPowerNotifications];
  systray_ready();
}

// registerPowerNotifications 监听系统关机/重启/注销通知：置位后 Go 侧退出流程
// （onBeforeClose）跳过 askStopServer 询问直接放行，避免模态对话框阻塞系统退出
// （否则 loginwindow 会显示"正在等待 dsh-systray 退出"，用户感知为应用阻止关机）。
- (void)registerPowerNotifications {
  NSNotificationCenter *nc = [[NSWorkspace sharedWorkspace] notificationCenter];
  [nc addObserver:self selector:@selector(systemWillPowerOff:)
             name:NSWorkspaceWillPowerOffNotification object:nil];
  [nc addObserver:self selector:@selector(systemSessionResigned:)
             name:NSWorkspaceSessionDidResignActiveNotification object:nil];
  // 快速用户切换切回（会话恢复）：复位标志，恢复正常的退出询问
  [nc addObserver:self selector:@selector(systemSessionBecameActive:)
             name:NSWorkspaceSessionDidBecomeActiveNotification object:nil];
}

- (void)systemWillPowerOff:(NSNotification *)note {
  systray_on_system_shutdown();
}

- (void)systemSessionResigned:(NSNotification *)note {
  systray_on_system_shutdown();
}

- (void)systemSessionBecameActive:(NSNotification *)note {
  systray_on_system_active();
}

- (void)applicationWillTerminate:(NSNotification *)aNotification {
  systray_on_exit();
}

// paddedStatusImage 菜单栏图标按 macOS 常规尺寸缩放并留白：
// 常见系统/微信类图标整体约 22pt 画布、图形约占 80%（四周留白），
// 让鲸鱼尽量占满菜单栏图标位（此前 60% 显得偏小）。
- (NSImage *)paddedStatusImage:(NSImage *)image {
  const CGFloat canvas = 22.0;
  const CGFloat glyphRatio = 0.8;
  NSImage *out = [NSImage imageWithSize:NSMakeSize(canvas, canvas) flipped:NO drawingHandler:^BOOL(NSRect rect) {
    CGFloat inset = rect.size.width * (1.0 - glyphRatio) / 2.0;
    NSRect drawRect = NSInsetRect(rect, inset, inset);
    [image drawInRect:drawRect fromRect:NSZeroRect
            operation:NSCompositingOperationSourceOver fraction:1.0
      respectFlipped:NO hints:nil];
    return YES;
  }];
  [out setTemplate:[image isTemplate]];
  return out;
}

- (void)setIcon:(NSImage *)image {
  statusItem.button.image = [self paddedStatusImage:image];
  [self updateTitleButtonStyle];
}

- (void)setTitle:(NSString *)title {
  statusItem.button.title = title;
  [self updateTitleButtonStyle];
}

-(void)updateTitleButtonStyle {
  if (statusItem.button.image != nil) {
    if ([statusItem.button.title length] == 0) {
      statusItem.button.imagePosition = NSImageOnly;
    } else {
      statusItem.button.imagePosition = NSImageLeft;
    }
  } else {
    statusItem.button.imagePosition = NSNoImage;
  }
}


- (void)setTooltip:(NSString *)tooltip {
  statusItem.button.toolTip = tooltip;
}

- (IBAction)menuHandler:(id)sender {
  NSNumber* menuId = [sender representedObject];
  systray_menu_item_selected(menuId.intValue);
}

- (void)add_or_update_menu_item:(MenuItem *)item {
  NSMenu *theMenu = self->menu;
  NSMenuItem *parentItem;
  //create_menu();
  if ([item->parentMenuId integerValue] > 0) {
    parentItem = find_menu_item(menu, item->parentMenuId);
    if (parentItem.hasSubmenu) {
      theMenu = parentItem.submenu;
    } else {
      theMenu = [[NSMenu alloc] init];
      [theMenu setAutoenablesItems:NO];
      [parentItem setSubmenu:theMenu];
    }
  }
  
  NSMenuItem *menuItem;
  menuItem = find_menu_item(theMenu, item->menuId);
  //item->shortcutKey
  if (menuItem == NULL) {
    menuItem = [theMenu addItemWithTitle:item->title action:@selector(menuHandler:) keyEquivalent:@""];
    [menuItem setRepresentedObject:item->menuId];
  }
  [menuItem setTitle:item->title];
  [menuItem setTag:[item->menuId integerValue]];
  [menuItem setTarget:self];
  [menuItem setToolTip:item->tooltip];
  if (item->disabled == 1) {
    menuItem.enabled = FALSE;
  } else {
    menuItem.enabled = TRUE;
  }
  if (item->checked == 1) {
    menuItem.state = NSControlStateValueOn;
  } else {
    menuItem.state = NSControlStateValueOff;
  }
}

NSMenuItem *find_menu_item(NSMenu *ourMenu, NSNumber *menuId) {
  NSMenuItem *foundItem = [ourMenu itemWithTag:[menuId integerValue]];
  if (foundItem != NULL) {
    return foundItem;
  }
  NSArray *menu_items = ourMenu.itemArray;
  int i;
  for (i = 0; i < [menu_items count]; i++) {
    NSMenuItem *i_item = [menu_items objectAtIndex:i];
    if (i_item.hasSubmenu) {
      foundItem = find_menu_item(i_item.submenu, menuId);
      if (foundItem != NULL) {
        return foundItem;
      }
    }
  }

  return NULL;
};

- (void) add_separator:(NSNumber*) menuId {
  [menu addItem: [NSMenuItem separatorItem]];
}

- (void) hide_menu_item:(NSNumber*) menuId {
  NSMenuItem* menuItem = find_menu_item(menu, menuId);
  if (menuItem != NULL) {
    [menuItem setHidden:TRUE];
  }
}

- (void) setMenuItemIcon:(NSArray*)imageAndMenuId {
  NSImage* image = [imageAndMenuId objectAtIndex:0];
  NSNumber* menuId = [imageAndMenuId objectAtIndex:1];

  NSMenuItem* menuItem;
  menuItem = find_menu_item(menu, menuId);
  if (menuItem == NULL) {
    return;
  }
  menuItem.image = image;
}

- (void) show_menu_item:(NSNumber*) menuId {
  NSMenuItem* menuItem = find_menu_item(menu, menuId);
  if (menuItem != NULL) {
    [menuItem setHidden:FALSE];
  }
}

- (void) create_menu {
  if(statusItem.menu == NULL){
    [statusItem setMenu:menu];
  }
}

- (void) set_menu_nil {
  if(statusItem.menu != NULL){
    [statusItem setMenu:NULL];
  }
}

- (void) reset_menu {
  [self->menu removeAllItems];
}

- (void) quit {
  [NSApp terminate:self];
}

- (void) statusOnClick:(NSButton *)btn {
    NSEvent *event = [NSApp currentEvent];
    if(event.type == NSEventTypeLeftMouseUp){
        systray_on_click();
    }else if(event.type == NSEventTypeRightMouseUp){
        systray_on_rclick();
    }
}

- (void) show_menu {
    // 菜单已在 applicationDidFinishLaunching 常驻挂接（statusItem.menu），系统点击即弹出。
    // 此方法仅作兼容入口（Go 侧 ShowMenuAsync 可能经此触发）：确保未脱钩即可，幂等无害。
    if (statusItem.menu == NULL) {
      [statusItem setMenu:menu];
    }
}

- (void) enable_on_click {
  [statusItem.button setAction:@selector(statusOnClick:)];
  [statusItem.button sendActionOn:(NSEventMaskLeftMouseUp|NSEventMaskRightMouseUp)];
}

@end

bool internalLoop = false;
SystrayAppDelegate *owner;

void setInternalLoop(bool i) {
	internalLoop = i;
}

void registerSystray(void) {
  if (!internalLoop) { // with an external loop we don't take ownership of the app
    return;
  }
  owner = [[SystrayAppDelegate alloc] init];
  [[NSApplication sharedApplication] setDelegate:owner];

  // A workaround to avoid crashing on macOS versions before Catalina. Somehow
  // SIGSEGV would happen inside AppKit if [NSApp run] is called from a
  // different function, even if that function is called right after this.
  if (floor(NSAppKitVersionNumber) <= /*NSAppKitVersionNumber10_14*/ 1671){
    [NSApp run];
  }
}

void nativeEnd(void) {
  systray_on_exit();
}

int nativeLoop(void) {
  if (floor(NSAppKitVersionNumber) > /*NSAppKitVersionNumber10_14*/ 1671){
    [NSApp run];
  }
  return EXIT_SUCCESS;
}

void nativeStart(void) {
  // AppKit 的 NSStatusItem / NSWindow 初始化只允许在主线程执行；Wails 集成时
  // onStartup 运行在其工作 goroutine（非主线程）→ 直接同步创建会抛
  // "NSWindow drag regions should only be invalidated on the Main Thread" 崩溃。
  // 统一派发到主队列：由 NSApplication 主运行循环线程执行初始化与系统回调。
  dispatch_async(dispatch_get_main_queue(), ^{
    owner = [[SystrayAppDelegate alloc] init];
    if (internalLoop) {
      // 仅自管 NSApplication 时才接管 delegate；外部循环（wails）下保留其自身
      // delegate（避免丢失 URL/文件打开等生命周期处理）。
      [[NSApplication sharedApplication] setDelegate:owner];
    }
    NSNotification *launched = [NSNotification
                                  notificationWithName: NSApplicationDidFinishLaunchingNotification
                                                object: [NSApplication sharedApplication]];
    [owner applicationDidFinishLaunching:launched];
  });
}

void runInMainThread(SEL method, id object) {
  [owner
    performSelectorOnMainThread:method
                     withObject:object
                  waitUntilDone: YES];
}

void setIcon(const char* iconBytes, int length, bool template) {
  NSData* buffer = [NSData dataWithBytes: iconBytes length:length];
  NSImage *image = [[NSImage alloc] initWithData:buffer];
  // 与 paddedStatusImage 画布一致（18pt）；绘制时按画布/图形比例居中缩放，Retina 下由
  // drawingHandler 自动按 2x 重采样，无需单独提供 @2x 资产。
  [image setSize:NSMakeSize(18, 18)];
  image.template = template;
  runInMainThread(@selector(setIcon:), (id)image);
}

void setMenuItemIcon(const char* iconBytes, int length, int menuId, bool template) {
  NSData* buffer = [NSData dataWithBytes: iconBytes length:length];
  NSImage *image = [[NSImage alloc] initWithData:buffer];
  [image setSize:NSMakeSize(16, 16)];
  image.template = template;
  NSNumber *mId = [NSNumber numberWithInt:menuId];
  runInMainThread(@selector(setMenuItemIcon:), @[image, (id)mId]);
}

void setTitle(char* ctitle) {
  NSString* title = [[NSString alloc] initWithCString:ctitle
                                             encoding:NSUTF8StringEncoding];
  free(ctitle);
  runInMainThread(@selector(setTitle:), (id)title);
}

void setTooltip(char* ctooltip) {
  NSString* tooltip = [[NSString alloc] initWithCString:ctooltip
                                               encoding:NSUTF8StringEncoding];
  free(ctooltip);
  runInMainThread(@selector(setTooltip:), (id)tooltip);
}

void add_or_update_menu_item(int menuId, int parentMenuId, char* title, char* tooltip, char* shortcutKey, short disabled, short checked, short isCheckable) {
  MenuItem* item = [[MenuItem alloc] initWithId: menuId withParentMenuId: parentMenuId withTitle: title withTooltip: tooltip withShortcutKey: shortcutKey withDisabled: disabled withChecked: checked];
  free(title);
  free(tooltip);
  runInMainThread(@selector(add_or_update_menu_item:), (id)item);
}

void add_separator(int menuId) {
  NSNumber *mId = [NSNumber numberWithInt:menuId];
  runInMainThread(@selector(add_separator:), (id)mId);
}

void hide_menu_item(int menuId) {
  NSNumber *mId = [NSNumber numberWithInt:menuId];
  runInMainThread(@selector(hide_menu_item:), (id)mId);
}

void show_menu_item(int menuId) {
  NSNumber *mId = [NSNumber numberWithInt:menuId];
  runInMainThread(@selector(show_menu_item:), (id)mId);
}

void reset_menu() {
  runInMainThread(@selector(reset_menu), nil);
}

void create_menu() {
  runInMainThread(@selector(create_menu), nil);
}

void set_menu_nil() {
  runInMainThread(@selector(set_menu_nil), nil);
}

void show_menu(){
  runInMainThread(@selector(show_menu), nil);
}

void enable_on_click(void) {
  runInMainThread(@selector(enable_on_click), nil);
}

void quit() {
  runInMainThread(@selector(quit), nil);
}
