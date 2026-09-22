@echo off
REM 2026-09-22 detached start (NOT "start /b"): /b shares the parent console,
REM so the controller dies when the parent shell is cleaned up (2 silent deaths).
cd /d F:\ZyBin\zyCtrlcenter
set ROBOT_CTRL_HOST=127.0.0.1
set ROBOT_CTRL_PORT=27200
set CTRL_AUTO_RESTORE=1
REM 2026-09-22 服务端在线数直连（livecount，参考 game_admin_web/origin_hqm 的 GMApi::onlineCount）
REM   接口：GET http://<游戏服>:8080/gm/online  → {"online_count":N,"success":true,"serverId":1000}
REM   无需 GM 授权；实测：192.168.0.201:8080 通；生产 47.96.8.240:8080 需在游戏服放行
REM   **** 2026-09-22 用户口径：暂不做"全服含真人"，先只用我们自己的握手数 → ENABLED=0；
REM        将来游戏服放行了把这里改回 1 即可（也可用 POST /api/livecount 在线切换，不用重启）****
REM   **** 白名单要放行的是本机"公网出口 IP 110.90.3.243"（不是内网 192.168.0.193）****
set CTRL_LIVECOUNT_ENABLED=0
set CTRL_LIVECOUNT_URL=http://47.96.8.240:8080
set CTRL_LIVECOUNT_SERVER_ID=1000

start "zyctrlcenter" /min "F:\ZyBin\zyCtrlcenter\zyctrlcenter.exe" --deploy "F:\ZyBin\xm\2d-xiyou-server\robot\deploy\single_robot_zy"
