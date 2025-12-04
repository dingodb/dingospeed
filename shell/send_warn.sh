#!/bin/bash

# nohup ./send_warn.sh > send_warn.log 2>&1 &
WEBHOOK=""
MONITOR_DIR=""
TIMEOUT=60
INTERVAL=60  # 监控间隔，60秒
alert_sent=false  # 标记是否已发送告警消息（初始未发送）

send_wechat_msg() {
    local msg_content="$1"
    local escaped_content=$(echo "$msg_content" | sed 's/"/\\"/g' | sed ':a;N;$!ba;s/\n/\\n/g')
    local json=$(printf '{
        "msgtype": "markdown",
        "markdown": {
            "content": "%s"
        }
    }' "$escaped_content")

    curl -s -X POST "$WEBHOOK" \
        -H "Content-Type: application/json" \
        -d "$json" > /dev/null 2>&1
}

# 初始提示
echo "[$(date +'%Y-%m-%d %H:%M:%S')] 启动目录监控服务"
echo "监控目录: $MONITOR_DIR"
echo "监控间隔: $INTERVAL 秒"
echo "超时时间: $TIMEOUT 秒"
echo "按 Ctrl+C 停止监控"
echo "========================================"

# 循环监控
while true; do
    start_time=$(date +'%Y-%m-%d %H:%M:%S')
    echo -e "\n[$start_time] 开始本次监控..."

    # 初始化临时文件
    output_file=$(mktemp)
    error_file=$(mktemp)
    monitor_success=false  # 标记本次监控是否成功

    # 检查目录是否存在
    if [ ! -d "$MONITOR_DIR" ]; then
        error_msg="::⚠️ 目录监控告警：$start_time\n"
        error_msg+="目录不存在或不是目录：$MONITOR_DIR"
        echo "$error_msg"
        # 仅当未发送过告警时，发送微信消息
        if [ "$alert_sent" = false ]; then
            send_wechat_msg "【目录监控告警】\n\n$error_msg"
            alert_sent=true  # 更新告警状态为已发送
        fi
    else
        # 执行监控命令（du -sh *），带超时控制
        if timeout "$TIMEOUT" bash -c "cd \"$MONITOR_DIR\" && df -h" > "$output_file" 2> "$error_file"; then
            monitor_success=true  # 监控成功
        else
            exit_code=$?
            # 构建错误信息
            error_msg="⚠️ 目录监控告警：$start_time\n"
            error_msg+="目录：$MONITOR_DIR\n"
            if [ $exit_code -eq 124 ]; then
                error_msg+="错误类型：命令执行超时\n"
                error_msg+="超时时间：$TIMEOUT 秒\n"
                error_msg+="可能原因：目录过大或IO性能问题"
            else
                error_msg+="错误类型：命令执行失败\n"
                error_msg+="退出码：$exit_code\n"
                error_msg+="可能原因：权限不足、目录内文件过多等"
                # 追加错误输出（可选，便于排查）
                error_detail=$(cat "$error_file" | head -5)  # 只取前5行错误信息，避免消息过长
                if [ -n "$error_detail" ]; then
                    error_msg+="\n错误详情：\n$error_detail"
                fi
            fi
            echo "$error_msg"
            # 仅当未发送过告警时，发送微信消息
            if [ "$alert_sent" = false ]; then
                send_wechat_msg "【目录监控告警】\n\n$error_msg"
                alert_sent=true  # 更新告警状态为已发送
            fi
        fi
    fi

    # 处理监控成功的情况
    if [ "$monitor_success" = true ]; then
        end_time=$(date +%s)
        start_timestamp=$(date -d "$start_time" +%s)
        duration=$((end_time - start_timestamp))

        echo "[$(date +'%Y-%m-%d %H:%M:%S')] 本次监控成功，耗时：$duration 秒"
        echo "本次监控输出："
        cat "$output_file"

        # 若之前发送过告警，现在发送恢复消息
        if [ "$alert_sent" = true ]; then
            recovery_msg="✅ 目录监控恢复通知：$(date +'%Y-%m-%d %H:%M:%S')\n"
            recovery_msg+="监控目录：$MONITOR_DIR\n"
            recovery_msg+="恢复状态：目录访问正常，du命令执行成功\n"
            recovery_msg+="本次监控耗时：$duration 秒"
            echo "$recovery_msg"
            send_wechat_msg "【目录监控恢复】\n\n$recovery_msg"
            alert_sent=false  # 重置告警状态为未发送
        fi
    fi

    # 清理临时文件
    rm -f "$output_file" "$error_file"

    # 等待下一次监控
    echo "[$(date +'%Y-%m-%d %H:%M:%S')] 等待 $INTERVAL 秒后进行下次监控..."
    sleep $INTERVAL
done

exit 0
