#!/bin/bash

#nohup ./monitor_container.sh > container_monitor.log 2>&1 &
CONTAINER_NAMES=("dingo-speed" "dingo-speed-offline")
WEBHOOK_URL=""


CHECK_INTERVAL=60
if ! command -v docker &> /dev/null; then
    echo "错误：未找到 'docker' 命令。请确保已安装 Docker 并在当前环境中可用。"
    exit 1
fi

if ! command -v curl &> /dev/null; then
    echo "错误：未找到 'curl' 命令。请安装 curl 后再运行脚本（如：yum install curl 或 apt install curl）。"
    exit 1
fi

echo "正在初始化多容器监控脚本..."
for container in "${CONTAINER_NAMES[@]}"; do
    LAST_START_TIME_FILE="/tmp/container_last_start_time_$container.txt"

    if [ -z "$(docker ps -q --filter "name=^/$container$")" ]; then
        echo "警告：容器 '$container' 不存在或未运行！将跳过该容器的监控"
        echo "提示：可通过 'docker ps --format '{{.Names}}'' 查看所有运行中的容器名称"
        continue
    fi

    INIT_START_TIME=$(docker inspect --format '{{.State.StartedAt}}' "$container" 2>/dev/null)
    if [ -z "$INIT_START_TIME" ]; then
        echo "警告：无法获取容器 '$container' 的启动状态，将跳过该容器的监控"
        continue
    fi

    echo "$INIT_START_TIME" > "$LAST_START_TIME_FILE"

    INIT_START_TIME_CN=$(echo "$INIT_START_TIME" | sed 's/Z$//' | date -d "$(cat -) UTC" +'%Y-%m-%d %H:%M:%S' 2>/dev/null)
    echo "  - 容器 '$container' 初始化完成，当前启动时间（UTC）：$INIT_START_TIME → 北京时间：$INIT_START_TIME_CN"
done

echo "初始化完成！脚本将每 $CHECK_INTERVAL 秒检查所有容器状态..."
echo "--------------------------------------------------"

while true; do
    for container in "${CONTAINER_NAMES[@]}"; do
        LOG_TIME=$(date +'%Y-%m-%d %H:%M:%S')
        LAST_START_TIME_FILE="/tmp/container_last_start_time_$container.txt"

        if [ ! -f "$LAST_START_TIME_FILE" ]; then
            continue
        fi

        CURRENT_START_TIME=$(docker inspect --format '{{.State.StartedAt}}' "$container" 2>/dev/null)

        if [ -z "$CURRENT_START_TIME" ]; then
            echo "$LOG_TIME - 警告：容器 '$container' 已不存在或无法访问！将停止监控该容器"
            rm -f "$LAST_START_TIME_FILE"  # 删除无效的临时文件
            continue
        fi

        LAST_START_TIME=$(cat "$LAST_START_TIME_FILE")

        if [ "$CURRENT_START_TIME" != "$LAST_START_TIME" ]; then
            echo "$LOG_TIME - 发现容器 '$container' 已重启！"

            OLD_START_TIME_UTC=$(echo "$LAST_START_TIME" | sed 's/Z$//')
            OLD_START_TIME_CN=$(date -d "$OLD_START_TIME_UTC UTC" +'%Y-%m-%d %H:%M:%S' 2>/dev/null)

            if [ -z "$OLD_START_TIME_CN" ]; then
                OLD_START_TIME_CN="$LAST_START_TIME"
            fi

            MESSAGE=$(cat <<EOF
{
    "msgtype": "text",
    "text": {
        "content": "⚠️ Docker容器重启通知 ⚠️\n\n容器名称：$container\n重启时间：$LOG_TIME\n旧启动时间：$OLD_START_TIME_CN"
    }
}
EOF
            )

            HTTP_RESPONSE=$(curl -s -o /dev/null -w "%{http_code}" \
                -X POST \
                -H "Content-Type: application/json" \
                -d "$MESSAGE" \
                "$WEBHOOK_URL")

            if [ "$HTTP_RESPONSE" -eq 200 ]; then
                echo "    -> 企业微信通知发送成功！"
                echo "    -> 推送内容：容器名称=$container，旧启动时间（UTC）=$LAST_START_TIME → 北京时间：$OLD_START_TIME_CN，重启时间：$LOG_TIME"
            else
                echo "    -> 警告：容器 '$container' 的企业微信通知发送失败，HTTP状态码：$HTTP_RESPONSE"
                echo "    -> 请检查 Webhook 地址是否正确，或企业微信机器人是否被禁用"
            fi

            echo "$CURRENT_START_TIME" > "$LAST_START_TIME_FILE"
            echo "--------------------------------------------------"
        else
            :
        fi
    done

    sleep "$CHECK_INTERVAL"
done
