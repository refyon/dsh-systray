@echo off
REM 鍦ㄧ湡瀹炴闈細璇濓紙鍙灞忓箷锛変笂鍙屽嚮鏈剼鏈紝閲嶆柊鐢熸垚 dsh-systray 鍏ㄩ儴鎴浘涓?README 涓诲浘銆?REM 姝ラ锛歝apture_shots.ps1锛堥€愰〉鎴睆锛屼繚瀛?PNG锛夆啋 split_about.py锛堝叧浜庨〉涓婁笅鍖哄煙鍒囧垎锛?REM        鈫?convert_webp.py锛堣浆 webp 骞舵竻鐞?PNG锛夆啋 make_hero.py锛堝悎鎴愪富鍥撅級鈫?convert_webp.py
setlocal
cd /d "%~dp0.."
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\capture_shots.ps1
if errorlevel 1 echo [capture] 澶辫触锛岃纭褰撳墠涓哄凡鐧诲綍鐨勫彲瑙佹闈細璇?
python scripts\convert_webp.py
python scripts\make_hero.py
python scripts\convert_webp.py
echo.
echo 瀹屾垚锛歞ocs\shots\*.webp 涓?docs\screenshot-hero.webp 宸叉洿鏂?pause