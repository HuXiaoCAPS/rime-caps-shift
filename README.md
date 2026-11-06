# RimeCapsShift

[![Build Windows executable](https://github.com/HuXiaoCAPS/rime-caps-shift/actions/workflows/build.yml/badge.svg)](https://github.com/HuXiaoCAPS/rime-caps-shift/actions/workflows/build.yml)

版本：1.3.1

在 Windows 上为 **小狼毫（Weasel / Rime）** 提供 CapsLock 兼容层：

- 当前台输入法是小狼毫且处于**中文模式**时，CapsLock 变成**左 Shift 锁存开关**：
  第一次按下锁定左 Shift，第二次按下解除；物理 CapsLock 的按下和松开都不会
  传递给系统。
- 小狼毫处于英文模式，或当前不是小狼毫时，CapsLock 保持系统原有行为。
- **右 Shift 短按**（可通过 `--shift-key` 改为左 Shift 或左右都监听）翻转程序
  维护的中英文状态；Shift 本身不拦截，继续交给 Rime。Ctrl+Space 也继续由
  小狼毫处理，程序只同步状态。
- 锁存时有两项视觉反馈：
  1. **托盘图标变色**：复用同一个托盘图标，默认米白 / 锁定琥珀色；
  2. **鼠标周围呼吸光晕**：半透明、点击穿透、不抢焦点，锁定解除即隐藏。
- 可选 `--beep` 在锁定/解除时播放系统提示音。
- 托盘右键菜单提供“开机自启动”开关（写入当前用户的 HKCU Run，不需要管理员）。
- `--no-tray` / `--hide-tray` 可完全隐藏托盘图标；此时用 `--quit` 退出。
- 不修改系统光标、不注入 CapsLock、不要求管理员权限、不需要驱动。

项目只使用 Python 标准库和 `ctypes`，不需要 `pywin32`。

## 文件

- `rime_caps_shift.py`：完整程序。双色托盘图标的 32×32 RGBA 数据内嵌在源码中，
  由项目图标源生成，运行时不需要额外图标文件。
- `assets/rime_caps_shift.ico`：多尺寸 Windows 可执行文件图标。
- `.github/workflows/build.yml`：GitHub Actions / Nuitka-Action 在线构建配置。
- `README.md`：本说明。

## 运行

要求 Windows 10/11、64 位 Python 3.9+：

```powershell
python rime_caps_shift.py
```

启动后任务栏通知区域会出现 RimeCapsShift 图标。右键菜单可查看当前输入法
与 Shift 锁定状态，或退出。

常用命令：

```powershell
# 只读诊断：输入法识别、托盘变色、光效配置与初始化结果
python rime_caps_shift.py --diagnose

# 请求正在运行的实例退出（会释放 Shift、隐藏光效）
python rime_caps_shift.py --quit

# 关闭鼠标光晕，只保留托盘变色
python rime_caps_shift.py --no-halo

# 自定义光晕颜色和直径
python rime_caps_shift.py --halo-color 00C8FF --halo-size 220

# 用左 Shift 短按切换（默认是右 Shift）
python rime_caps_shift.py --shift-key left

# 左右 Shift 都监听，阈值改为 400ms
python rime_caps_shift.py --shift-key both --shift-threshold 400

# 完全隐藏托盘图标（依旧后台运行，用 --quit 退出）
python rime_caps_shift.py --no-tray

# 已隐藏托盘图标时，让运行中的实例重新显示托盘
python rime_caps_shift.py --show-tray

# 锁存/解除时播放系统提示音
python rime_caps_shift.py --beep

# 调试：不显示托盘，把日志输出到控制台和文件
python rime_caps_shift.py --no-tray --debug --log-file .\rime_caps_shift.log
```

正常运行时可 `Ctrl+C` 退出。

## 小狼毫配置前提

本程序绕过的是下面这个已知问题组合：

```yaml
# default.custom.yaml 或相关 schema 的 patch
ascii_composer/good_old_caps_lock: true
ascii_composer/switch_key/Caps_Lock: noop
```

另外请保持 `Shift_L` 为 `noop`（空白 Shift 不触发小狼毫内部切换）：

```yaml
ascii_composer/switch_key/Shift_L: noop
```

本程序注入的是**左 Shift**。如果左 Shift 被配置成切换中英文，注入事件可能反过来
触发模式变化。修改配置后需要重新部署小狼毫。

### Shift 键选择必须与 Rime 一致

本程序的 Shift 短按判定只翻转程序自己的 `is_rime_chinese` 状态，**不会代替 Rime
切换内部模式**。因此：

- 默认 `--shift-key right`：Rime 的右 Shift 应负责中英文切换（例如
  `Shift_R: commit_code`）；
- `--shift-key left`：Rime 的左 Shift 必须负责切换；如果仍是 `Shift_L: noop`，
  Rime 不会切换，程序状态会与实际模式失步；
- `--shift-key both`：左右 Shift 都可以，但两者都应交给 Rime 切换模式。

本程序注入左 Shift，因此保留 `Shift_L: noop` 很重要；如果想让左 Shift 同时
切换中英文，则 CapsLock 锁存注入的 Shift 也可能触发 Rime 切换，需要自行权衡。

## 识别原理

单靠 HKL 描述并不可靠。当前实测的小狼毫 0.17.4 是一个 **TSF 文本服务**：

- 前台 HKL 是通用的 `0x08040804`；
- `ImmGetDescriptionW` 返回空字符串；
- 微软拼音等中文输入法也可能共用同一个 HKL。

因此 `WeaselDetector` 采用两层识别：

1. **HKL 兼容路径**
   `GetKeyboardLayout(GetWindowThreadProcessId(GetForegroundWindow()))`
   取 HKL，再调用 `ImmGetDescriptionW` 和 `ImmGetIMEFileNameW`，匹配
   `Weasel`、`小狼毫`、`Rime` 关键字。对应 `is_weasel_hkl(hkl)`。
2. **TSF 补充路径（只读）**
   从注册表读取已安装的 TSF 配置描述和 CLSID，用当前活动的
   `ITfInputProcessorProfiles::GetActiveLanguageProfile` 判断活动配置是否
   为 Weasel。只有前台 HKL 属于中文语言时才采用这条路径，因为切换到英语
   键盘布局后 TSF 仍可能报告 Weasel 为“活动配置”。

TSF 查询只读注册表和 COM 状态，不会修改注册表或输入法配置，也不注入其他进程。

前台输入法通过 250ms 定时器轮询；切换输入法后最多约 250ms 更新状态。
**TSF 查询只在轮询和启动阶段进行，键盘钩子回调内完全不做 TSF/COM 查询。**

## 状态机

- `is_rime_chinese`：程序维护的小狼毫中文状态；只由 `_flip_chinese()` 修改。
- `_weasel_foreground`：前台是否为小狼毫；只由 250ms 轮询维护。
- `shift_latched`：左 Shift 是否处于锁存按住状态。
- 启动时：前台是小狼毫 → `is_rime_chinese=True`，Shift 未锁定。
- 前台从非小狼毫切换到小狼毫 → `True`，Shift 未锁定，并清空旧 Shift 周期。
- 前台从小狼毫切换到其他输入法或英语键盘 → `False`，解除 Shift 锁定、
  释放 Shift、隐藏光效、托盘恢复默认色，并清空旧 Shift 周期。
- 被监听的 Shift 短按：仅当 `_weasel_foreground` 为真、`_rshift_used` 为假、
  时长小于 `--shift-threshold` 时翻转 `is_rime_chinese`。Shift 本身不吞键，
  也不主动锁存；若翻转导致离开中文模式，`_flip_chinese()` 会按原有安全逻辑
  释放已经锁存的注入 Shift。
- 任何非 Shift 键按下（CapsLock、Space、字母键等）都会把当前 Shift 周期标记为
  “已污染”，该次 Shift 释放不翻转。
- Ctrl+Space：只读取轮询缓存的 `_weasel_foreground`；为假时不翻转。Space 按下
  时若**物理**左/右 Shift 仍按住（`_user_lshift_down` / `_user_rshift_down`），
  跳过翻转；CapsLock 锁存注入的左 Shift 不算物理 Shift。Ctrl+Space 不拦截。
- 中文模式下第一次按 CapsLock → 吞掉事件，发送左 Shift Down 并锁定；不修改
  `is_rime_chinese`。
- 中文模式下第二次按 CapsLock → 吞掉事件，发送左 Shift Up 并解除锁定；不修改
  `is_rime_chinese`。
- 松开 CapsLock 的物理按键不会解除锁定，只吞掉 key-up。
- 非中文模式/非小狼毫时 CapsLock 正常透传。
- 自动重复不会重复触发锁存或翻转。
- 切换状态或退出时发送 Shift Up，并隐藏视觉反馈。

## 键盘钩子安全边界

`WH_KEYBOARD_LL` 回调内只进行内存状态判断、`GetAsyncKeyState`、`time.monotonic`
和必要的 `SendInput`。物理左/右 Shift 的按下/释放由钩子无条件维护，与
`--shift-key` 的监听选择无关；因为钩子入口已过滤 `LLKHF_INJECTED`，CapsLock
锁存注入的 Shift 不会污染物理状态。以下操作全部被移到钩子外的 50ms/250ms
定时器中：

- TSF/COM 查询（`_weasel_foreground` 完全由轮询维护）；
- `logging` 日志；
- 托盘图标刷新、光效显示/隐藏；
- `MessageBeep` 提示音。

Shift/CapsLock 事件只把待处理状态标记为 dirty，由 `flush_pending_ui()` 在定时器中
统一消费。这样不会在低级键盘钩子内触发 COM 或 UI 操作。视觉/提示音更新最多
延迟到下一个定时器 tick：启用光效时约 50ms，`--no-halo` 时约 250ms。

## 视觉反馈

### 托盘图标变色

托盘图标仍使用唯一的 `uID=1`，不新增第二个通知图标。锁存状态变化时通过
`Shell_NotifyIconW(NIM_MODIFY)` 更新同一个托盘项：

- 未锁存：米白背景 RS 图标；
- 锁存：琥珀色背景 RS 图标；
- 切换输入法/解除锁存/退出时恢复默认图标。

图标由提供的 `rcs_icon.png` 生成两张 32×32 RGBA 图，压缩后内嵌在 Python
源文件中，运行时通过 `CreateIconIndirect` 创建 HICON；不依赖外部 `.ico`。
生成或创建失败时回退到系统应用图标，并记录 warning。

### 鼠标呼吸光晕

锁存且当前处于小狼毫中文模式时，显示一个跟随鼠标的光晕：

- 使用 `WS_EX_LAYERED | WS_EX_TRANSPARENT | WS_EX_TOOLWINDOW |
  WS_EX_NOACTIVATE | WS_EX_TOPMOST` 的分层窗口；
- `WM_NCHITTEST` 返回 `HTTRANSPARENT`，点击穿透；
- `WS_EX_NOACTIVATE` + `SW_SHOWNOACTIVATE`，不抢焦点；
- 使用 `UpdateLayeredWindow` 显示预渲染的 32bpp 光晕位图，呼吸只改变
  `SourceConstantAlpha` 和窗口位置，不每帧重绘全屏；
- 光效计时器约 50ms（20 FPS），仅在显示时更新；未锁存/切离小狼毫/退出时
  立即 `SW_HIDE`，隐藏期间不做绘制；
- 光晕的位图、内存 DC 在初始化时创建并缓存，退出时释放；
- 光效窗口初始化失败只记录 warning，托盘变色和键盘映射仍然工作。

默认启用，可用以下参数调整：

```powershell
--no-halo              # 完全关闭光晕
--halo-color RRGGBB    # 六位十六进制，默认 FFB347（琥珀）
--halo-size PX         # 物理像素直径，默认 96，限制 32..512
```

启动时优先启用 **Per-Monitor V2 DPI 感知**，失败时依次尝试 Per-Monitor V1
和 System DPI aware，保证 `GetCursorPos` 和窗口坐标使用物理像素，支持跨显示器。

### 提示音

`--beep` 打开后，锁定和解除时各调用一次 `MessageBeep(MB_OK)`；默认关闭。

## 开机自启动

托盘右键菜单中有“开机自启动”开关，勾选状态表示当前用户是否已设置。

- 写入位置：`HKCU\Software\Microsoft\Windows\CurrentVersion\Run\RimeCapsShift`
- 使用当前启动参数（会过滤 `--debug`、`--log-file`、`--quit`、`--diagnose`）
- 普通用户权限，不需要管理员，不需要计划任务
- 再次点击可删除该 Run 值
- 如果移动了 EXE/脚本或修改了启动参数，请重新点击两次开关以刷新 Run 值
- 如果当前以 `--no-tray` 运行，没有托盘菜单；可以先运行
  `rime_caps_shift.py --show-tray` 让运行中的实例恢复托盘，再操作开关

> 这是本项目唯一的注册表写入行为，且只在用户主动点击托盘菜单“开机自启动”时
> 发生。其他时候只读取注册表。

## 隐藏托盘图标

有两种方式：

1. **启动时隐藏**：`--no-tray`（别名 `--hide-tray`）
2. **运行时隐藏**：托盘右键菜单 → **隐藏托盘图标**

隐藏后没有托盘菜单，可以恢复或退出：

```powershell
# 让运行中的实例重新创建托盘图标
python rime_caps_shift.py --show-tray

# 直接退出后台实例
python rime_caps_shift.py --quit
```

如果希望开机自启动时也隐藏托盘，可以在
`HKCU\Software\Microsoft\Windows\CurrentVersion\Run\RimeCapsShift` 的命令末尾
加上 `--no-tray`；也可以先正常启动、从托盘开启自启动，然后编辑该 Run 值。

## 代码结构：单文件是否需要拆分

目前 `rime_caps_shift.py` 约 2500 行，仍然是一个自包含单文件；这是刻意保留的：

- Nuitka 对单文件入口和多文件包都能正常打包，拆分不会让 EXE 更小或更快；
- 单文件便于分发、审查和在线构建；
- 当前功能边界清楚，暂时没有必要为了“看起来工程化”而拆分。

如果后续继续增加配置持久化、托盘多语言、更多输入法后端等，再按
`win32_api` / `weasel_detector` / `visuals` / `keyboard_handler` / `app`
拆成包会更易维护；现阶段不建议拆。

## GitHub Actions 在线构建（推荐）

仓库已配置 [`.github/workflows/build.yml`](.github/workflows/build.yml)，不需要在
本地安装 Nuitka、Visual Studio 或 MinGW：

- Runner：`windows-latest`
- Python：`3.12` x64
- 构建器：`Nuitka/Nuitka-Action@v1.5`
- Nuitka：固定 `4.3.4`
- 模式：`standalone`
- 启动优化：`python-flag: no_site`
- 控制台：`windows-console-mode: disable`（GUI 程序，无黑框）
- 图标：`assets/rime_caps_shift.ico`
- 排除：`nofollow-import-to` 列表（tkinter、unittest、ssl、http、xml、json、
  sqlite3、multiprocessing、asyncio 等未使用模块）
- 产物：`RimeCapsShift-windows-x64.zip` + `.sha256`

触发方式：

1. 手动：GitHub 仓库页面 → **Actions** → **Build Windows executable** →
   **Run workflow**。
2. 自动：推送 `main` 分支或提交 Pull Request。
3. 打 tag（如 `v1.3.1`）push 后也会构建。

构建完成后，在该次 workflow 的 **Artifacts** 中下载
`RimeCapsShift-windows-x64-standalone`，解压后运行 `RimeCapsShift.exe`。
Standalone 目录里的 DLL 必须和 EXE 放在一起，不能只复制 EXE。Artifact 默认
保留 30 天。

校验下载的 zip：

```powershell
Get-FileHash .\RimeCapsShift-windows-x64.zip -Algorithm SHA256
# 与 RimeCapsShift-windows-x64.zip.sha256 中的值比较
```

推送 `v*` tag 时，workflow 还会自动创建对应的 **GitHub Release**，附带
`RimeCapsShift-windows-x64.zip` 和 `.sha256`：

```powershell
git tag v1.3.1
git push origin v1.3.1
```

Release 页面：
<https://github.com/HuXiaoCAPS/rime-caps-shift/releases/tag/v1.3.1>

Nuitka-Action 本身不是编译器，它会在 GitHub 的 Windows runner 上安装指定
版本的 Nuitka 并调用 MSVC 构建。`assume-yes-for-downloads: true` 允许 Nuitka
自动获取它需要的构建辅助工具；这与本项目运行时无额外依赖无关。

等价的本地核心命令大致是：

```powershell
python -m nuitka --standalone --python-flag=no_site `
  --windows-console-mode=disable `
  --windows-icon-from-ico=assets\rime_caps_shift.ico `
  --output-dir=build rime_caps_shift.py
```

> `--nofollow-import-to` 只能排除确定不会运行的模块；排错会导致 EXE 启动时
> `ModuleNotFoundError`。完整列表见 workflow 文件。

> 当前 workflow 在 `v*` tag 上自动创建/更新 GitHub Release；普通 `main` push
> 和 Pull Request 只上传 Actions Artifact。

## 使用 Nuitka 本地构建

安装 Nuitka（本机开发环境使用 pixi，也可用 pip）：

```powershell
# 当前目录没有 pixi 工程时：
#   pixi init
#   pixi add python=3.12 nuitka
# 或者全局安装：
#   pixi global install nuitka
```

如果使用 pixi 工程：

```powershell
pixi run python -m nuitka --standalone --python-flag=no_site `
  --windows-console-mode=disable `
  --windows-icon-from-ico=assets\rime_caps_shift.ico `
  --assume-yes-for-downloads --output-dir=build `
  rime_caps_shift.py
```

如果用普通 Python 环境：

```powershell
python -m pip install nuitka
python -m nuitka --standalone --python-flag=no_site `
  --windows-console-mode=disable `
  --windows-icon-from-ico=assets\rime_caps_shift.ico `
  --assume-yes-for-downloads --output-dir=build `
  rime_caps_shift.py
```

生成目录通常位于 `build\rime_caps_shift.dist`，主程序是其中的
`rime_caps_shift.exe`。分发时必须带上整个目录，不能只复制 EXE。

说明：

- `standalone` 比 `onefile` 少一个引导进程，启动时不需要解压到临时目录；代价是
  分发的是目录而不是单文件。
- `--nofollow-import-to` 用于排除确定不会使用的标准库模块，减少产物体积和启动
  加载；不要把 `ctypes`、`logging`、`argparse`、`subprocess`、`winreg` 等实际
  使用的模块加进去。
- Nuitka 首次编译需要 C 编译器。已安装 Visual Studio Build Tools 时 Nuitka
  会自动查找；否则可加 `--mingw64 --assume-yes-for-downloads` 让 Nuitka
  下载 MinGW64。MinGW64 下载体积较大（数百 MB），请按需使用。
- 本项目不需要管理员权限，不需要添加 `--windows-uac-admin`。
- `--windows-console-mode=disable` 构建下没有控制台；启动失败会弹出错误对话框。
  运行日志可通过 `--log-file` 指定。
- Standalone EXE 或目录移动后，开机自启动的 Run 值需要重新开关一次以刷新路径。
- 未签名的 EXE 可能触发 SmartScreen 或被杀毒软件提示，这是打包器的常见现象；
  可对 EXE 做签名，或把目录加入白名单。

## 内存占用说明

先区分两件事：

- **onefile** 运行时有两个进程：Nuitka 引导进程负责解压/启动，子进程才是 Python
  程序。任务管理器里可能只看到一个，也可能看到两个。
- **standalone** 只有一个进程，不需要每次启动解压到临时目录。它不会让 Python
  解释器消失，但可以去掉引导进程的开销，并改善启动速度。

`--nofollow-import-to` 主要减少分发体积和启动时需要扫描/加载的模块；对已经加载
的 CPython 运行时、ctypes、GDI/USER32 这些内存没有决定性影响。不要期待从 13MB
降到 3MB。

本版已经做的低风险优化：

- 改为 `standalone`，去掉 onefile 引导进程；
- `python-flag: no_site`，不执行 `site.py`；
- 删除未使用的 `typing` 导入（项目使用 `from __future__ import annotations`）；
- `subprocess`、`base64`、`zlib` 改为按需导入；
- `--nofollow-import-to` 排除 50 个未使用的标准库模块。

想进一步显著降低内存，只剩这些方向：

1. **不用 Python 重写**：C/C++/Rust/C# 原生实现可以轻松做到几 MB，但开发量和
   维护成本高，当前项目不值得。
2. **关闭光效**：`--no-halo` 只省很小的 GDI 位图内存，不是大头。
3. **减少常驻 Python 模块**：例如去掉 `logging`/`argparse`，用极简实现替代；
   能省一点，但会牺牲可维护性和日志能力。
4. **接受现状**：13MB 级别的常驻内存对一个 Python + Win32 钩子程序并不夸张，
   而且已经低于小狼毫服务本身。

## 已知限制与注意事项

1. **无法直接读取小狼毫内部中英文状态**
   `is_rime_chinese` 现在由 Ctrl+Space 和 Shift 短按事件驱动，但仍无法查询
   librime 的真实模式。**鼠标点击小狼毫状态栏切换中英文、使用未监听的快捷键
   切换中英文，本程序无法感知**，会导致状态失步。此时用被监听的 Shift 短按或
   Ctrl+Space 切一次，或切到别的输入法再切回小狼毫，可以重新同步。
2. **Shift 键必须与 Rime 配置一致**
   程序只翻转自己的状态，不代替 Rime 切换。若 Rime 没有把所选 Shift 配置为
   中英文切换（例如 `Shift_L: noop` 却使用 `--shift-key left`），程序状态会与
   Rime 实际模式不一致。默认 `right` 需要右 Shift 在 Rime 中负责切换。
3. **识别关键字可能需要调整**
   关键字定义为 `rime_caps_shift.py` 中的 `WEASEL_KEYWORDS`；另有固定兜底
   CLSID `WEASEL_DEFAULT_CLSID`。先运行 `--diagnose`，如果 TSF 活动配置为
   否、HKL 描述也为空，说明识别失败；可把诊断中显示的描述加入关键字，或把
   真实 CLSID 加入候选。
4. **UIPI/权限**
   程序以普通权限运行时，`SendInput` 不能向以管理员权限运行的窗口注入按键。
   如果需要在管理员程序中生效，需要以相同权限启动本程序；本项目不会主动请求
   管理员权限。
5. **Secure Desktop**
   UAC 提示、登录界面、锁屏等安全桌面不在普通桌面钩子的覆盖范围内。
6. **部分游戏/反作弊/远程桌面环境**
   使用 Raw Input/DirectInput 或主动屏蔽低级键盘钩子的程序可能收不到映射效果。
7. **全屏独占游戏**
   独占全屏模式可能看不到分层光晕窗口，这是预期现象；托盘变色仍会生效。
8. **Ctrl+Space 的状态同步**
   程序不拦截 Ctrl+Space，只在小狼毫前台时翻转自己的状态。Space 按下时若
   **物理** Shift 按住会跳过翻转；CapsLock 锁存的注入 Shift 不阻止 Ctrl+Space。
   如果小狼毫没有把 Ctrl+Space 配置为切换中英文，或当前应用吞掉了该组合键，
   可能出现状态与实际不一致。
9. **物理 Shift 同时按下**
   程序发送的是独立的左 Shift 注入事件。若你正物理按住 Shift，再按 CapsLock，
   可能出现修饰键状态交叉，属于少见边界情况。
10. **托盘依赖 Explorer**
    如果没有任务栏通知区域，托盘图标可能不会出现；可用 `--quit` 或 `Ctrl+C` 退出。
    `--no-tray` 可在无 Explorer 环境运行，光效仍独立工作。
11. **异常终止**
    如果进程被强制结束（任务管理器结束进程、崩溃且未执行清理），注入的 Shift
    可能暂时保持按下；按一下并松开物理 Shift，或运行 `--quit`/重新启动程序，
    通常可以恢复。正常退出路径都会发送 Shift Up 并隐藏光效。
12. **单实例**
    程序使用 `Local\RimeCapsShift.SingleInstance` 互斥量，同一会话只允许一个实例。
13. **注册表与系统修改**
    不写系统级注册表、不修改输入法配置、不注入其他进程、不安装驱动、不修改
    系统光标。仅当用户主动点击托盘菜单“开机自启动”时，写入当前用户的
    `HKCU\Software\Microsoft\Windows\CurrentVersion\Run\RimeCapsShift`；
    再次点击会删除该值。其他时候只读取输入法注册信息、前台 HKL、TSF 活动配置
    和光标位置。

## 为什么没有用 pywin32 或第三方 GUI 库

Win32 钩子、托盘、GDI 光效和 TSF COM 查询都能用 `ctypes` 完成。使用额外依赖
会增加 Nuitka 打包和部署复杂度；纯 `ctypes` 版本运行时只需要标准库。

## 验证建议

1. 运行 `python rime_caps_shift.py --diagnose`，确认：
   - “最终前台判定：小狼毫”；
   - “Shift 监听键”和“Shift 阈值”；
   - “钩子内 TSF 查询：已移除”；
   - “托盘变色：可用”；
   - “光效：可用”。
2. 启动程序，在小狼毫中文模式下：
   - 第一次按 CapsLock：Shift 保持按下、托盘图标变琥珀色、鼠标周围出现光晕；
   - 松开 CapsLock：Shift 仍保持、光晕仍显示；
   - 输入字母应为大写；
   - 第二次按 CapsLock：Shift 释放、托盘图标恢复米白、光晕隐藏；
   - CapsLock 不应切换系统 Caps Lock 锁定。
3. **Shift 短按切中英文**：确认 Rime 已把所选 Shift 配置为模式切换后：
   - 短按右 Shift（默认）：程序状态翻转，视觉反馈在定时器刷新后更新；
   - 按住右 Shift 再按 A 后释放：`_rshift_used` 被置位，不应误翻转；
   - 长按超过 `--shift-threshold`（默认 500ms）后释放：不应翻转。
4. **Ctrl+Space**：
   - 小狼毫前台时按 Ctrl+Space：状态翻转一次；自动重复不会多次翻转；
   - Ctrl+Shift+Space（物理 Shift）：跳过翻转；
   - CapsLock 锁存左 Shift 后直接按 Ctrl+Space：应正常翻转并释放锁存，因为
     注入的 Shift 不算物理 Shift。
5. **输入法切换**：保持 Shift 按下，用 Win+Space 切走；旧的 Shift 周期被清空，
   松手后不应再翻转。切回小狼毫后状态重置为中文。
6. 托盘右键退出或运行 `--quit`，确认没有 Shift 卡住、光效窗口消失。
7. 用 `--no-halo` 验证托盘变色仍工作；用 `--halo-color 00C8FF --halo-size 220`
   验证颜色/尺寸；用 `--beep` 验证提示音。
