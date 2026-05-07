import os
import requests
import time
from datetime import datetime, timezone
from huggingface_hub import snapshot_download

# ===================== 【配置区】=====================
HF_TOKEN = ""
HF_ENDPOINT = "http://localhost:8091"

# 要监控的作者数组
AUTHORS = [
    "deepseek-ai"
]

CACHE_DIR = "/root/hub-download/script"  # 缓存目录
INTERVAL = 600                  # 轮询间隔（秒）

# ===================== 【企微机器人配置】=====================
WECHAT_WEBHOOK = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=0bafcb30-c92f-4104-b667-14baf7c77d1c"  # 企微机器人 Webhook 地址，留空则不发送

# ===================== 【代理配置】=====================
# 如果你需要代理访问 HF API，在这里填写
# 不需要代理就留空即可
# HTTP_PROXY = "http://100.64.1.68:1080"
# HTTPS_PROXY = "http://100.64.1.68:1080"
HTTP_PROXY = ""
HTTPS_PROXY = ""

# =====================================================

# 环境变量
os.environ["HF_ENDPOINT"] = HF_ENDPOINT
os.environ["HF_TOKEN"] = HF_TOKEN
os.environ["HUGGINGFACE_HUB_CACHE"] = CACHE_DIR
os.environ["TRANSFORMERS_CACHE"] = CACHE_DIR
os.makedirs(CACHE_DIR, exist_ok=True)

# 构建代理字典
proxies = {}
if HTTP_PROXY:
    proxies["http"] = HTTP_PROXY
if HTTPS_PROXY:
    proxies["https"] = HTTPS_PROXY

# ===================== 企微消息发送 =====================
def send_data(content: str):
    """参考 Go 端 util.SendData，向企微机器人发送文本消息"""
    if not WECHAT_WEBHOOK:
        return
    msg = {
        "msgtype": "text",
        "text": {"content": content},
    }
    try:
        resp = requests.post(
            WECHAT_WEBHOOK,
            json=msg,
            timeout=3,
        )
        result = resp.json()
        if result.get("errcode", 0) != 0:
            print(f"⚠️ 企微消息发送失败：{result.get('errmsg', 'unknown')}")
    except Exception as e:
        print(f"⚠️ 企微消息发送异常：{e}")

# ===================== 核心逻辑 =====================
# 记录脚本启动时间（只下载此时间之后创建的新模型）
SCRIPT_START_TIME = datetime.now(timezone.utc)

# 已经下载过的模型（内存记录，不写盘）
downloaded_models = set()

print("🚀 多作者模型监控已启动（仅下载新发布模型）")
print(f"👥 监控作者：{AUTHORS}")
print(f"📂 缓存目录：{CACHE_DIR}")
print(f"⏱️  脚本启动时间（UTC）：{SCRIPT_START_TIME}")
print(f"🔄 每 {INTERVAL} 秒检查一次\n")
# =====================================================
# 主循环
while True:
    for author in AUTHORS:
        try:
            # 获取作者最新模型
            url = f"https://huggingface.co/api/models?author={author}&sort=lastModified&direction=-1"
            resp = requests.get(url, timeout=10, proxies=proxies)
            resp.raise_for_status()
            models = resp.json()

            for model in models[:3]:  # 取最新3个足够
                model_id = model["id"]
                created_at = model.get("createdAt", "")
                if not created_at:
                    continue

                # 解析 HF 返回的 UTC 时间
                try:
                    create_time = datetime.fromisoformat(created_at.replace("Z", "+00:00"))
                except:
                    continue

                # ===================== 判断规则 =====================
                # 1. 模型创建时间 晚于 脚本启动时间
                # 2. 还没下载过
                # =====================================================
                if create_time > SCRIPT_START_TIME and model_id not in downloaded_models:
                    print(f"\n🆕 发现新模型: {model_id}")
                    print(f"   模型发布时间: {created_at}")

                    # 下载前通知
                    send_data(f"🆕 发现新模型，开始下载：{model_id}\n模型发布时间：{created_at}")

                    try:
                        # 开始缓存
                        snapshot_download(
                            repo_id=model_id,
                            repo_type="model",
                            cache_dir=CACHE_DIR,
                            token=HF_TOKEN,
                            endpoint=HF_ENDPOINT,
                            resume_download=True
                        )
                        print(f"✅ 缓存完成：{model_id}")
                        downloaded_models.add(model_id)

                        # 下载成功通知
                        send_data(f"✅ 模型下载完成：{model_id}")

                    except Exception as e:
                        print(f"❌ 缓存失败：{str(e)}")
                        # 下载失败通知
                        send_data(f"❌ 模型下载失败：{model_id}\n错误：{str(e)}")

        except Exception as e:
            print(f"⚠️ {author} 获取失败：{str(e)}")
            send_data(f"❌  {author} 模型获取失败：{str(e)}")

    time.sleep(INTERVAL)