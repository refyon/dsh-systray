# 从源码中提取“应用内固定文案”所用字符，用 Noto Sans SC（本系统已装 NotoSansSC-VF.ttf）
# 生成静态 Regular/Bold 子集字体。新增页面/控件后运行本脚本即可更新字体打包。
# 输出：assets/NotoSansSC-Regular.ttf、assets/NotoSansSC-Bold.ttf
import re, glob, os, sys
from fontTools.ttLib import TTFont
from fontTools.varLib import instancer
from fontTools import subset

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SRC = os.path.join(ROOT)  # 项目根（*.go / *.m 在此或其子目录）
OUT = os.path.join(ROOT, 'assets')
os.makedirs(OUT, exist_ok=True)
VF = r'C:\Windows\Fonts\NotoSansSC-VF.ttf'

# ---- 收集字符：ASCII 可打印 + 源码字符串里的中文/全角 ----
chars = set(chr(c) for c in range(0x20, 0x7F))
def add_cjk_run(s):
    for ch in s:
        if ('\u4e00' <= ch <= '\u9fff') or ('\u3000' <= ch <= '\u303f') or ('\uff00' <= ch <= '\uffef'):
            chars.add(ch)
pat = re.compile(r'"(?:[^"\\]|\\.)*"')
back = re.compile(r'`([^`]*)`')
def scan(path):
    text = open(path, encoding='utf-8').read()
    for m in pat.finditer(text):
        add_cjk_run(m.group(0)[1:-1])
    for m in back.finditer(text):
        add_cjk_run(m.group(1))
for ext in ('*.go', '*.m'):
    for p in glob.glob(os.path.join(SRC, '**', ext), recursive=True):
        scan(p)
# 固定补充：常用符号（时间/百分号等已在 ascii，此处补中文常用标点）
for c in '，。！？；：、（）【】《》“”‘’—…％℃': chars.add(c)
text = ''.join(sorted(chars))
print('glyph count (approx):', len(chars))

# ---- 从可变字体实例化静态 Regular/Bold 并子集化 ----
for w, out_name in [(400, 'NotoSansSC-Regular.ttf'), (700, 'NotoSansSC-Bold.ttf')]:
    f = TTFont(VF)
    inst = instancer.instantiateVariableFont(f, {'wght': w}, inplace=False)
    ss = subset.Subsetter(options=subset.Options(drop_tables=[], name_IDs=['*']))
    ss.populate(text=text)
    ss.subset(inst)
    out = os.path.join(OUT, out_name)
    inst.save(out)
    print(out_name, os.path.getsize(out)//1024, 'KB')
print('done')
