package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"dingospeed/pkg/config"

	"go.uber.org/zap"
)

func SendData(content string) {
	msg := Message{
		MsgType: "text",
		Text: TextMsg{
			Content: content,
		},
	}

	if err := sendMessage(msg); err != nil {
		zap.S().Errorf("微信机器人发送消息失败: %v", err)
	}

	return
}

// Response 企业微信 API 返回结果
type WxResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

// Message 通用消息结构
type Message struct {
	MsgType string  `json:"msgtype"`
	Text    TextMsg `json:"text,omitempty"`
}

// TextMsg 文本消息
type TextMsg struct {
	Content             string   `json:"content"`
	MentionedList       []string `json:"mentioned_list,omitempty"`
	MentionedMobileList []string `json:"mentioned_mobile_list,omitempty"`
}

// sendMessage 发送消息的基础方法
func sendMessage(msg Message) error {
	// 转换为 JSON
	jsonData, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("消息序列化失败: %v", err)
	}

	// 创建请求
	req, err := http.NewRequest("POST", config.SysConfig.GetWebhook(), bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("创建请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// 发送请求
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("发送请求失败: %v", err)
	}
	defer resp.Body.Close()

	// 读取响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取响应失败: %v", err)
	}

	// 解析响应
	var result WxResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("解析响应失败: %v, 响应内容: %s", err, string(body))
	}

	if result.ErrCode != 0 {
		return fmt.Errorf("发送消息失败: %s", result.ErrMsg)
	}

	return nil
}
