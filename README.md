# DingoSpeed
English | [简体中文](README-zh_CN.md)

DingoSpeed is a self-hosted Hugging Face mirror service designed to provide users with a convenient and efficient solution for accessing and managing model resources. Through local mirroring, users can reduce their reliance on remote Hugging Face servers, improve resource acquisition speed, and achieve local storage and management of data.

# Product Features
DingoSpeed has the following main product features:
* Mirror Acceleration: Cache the resources downloaded for the first time. When the client makes a subsequent request, the data will be read from the cache and returned, greatly improving the download rate.
* Convenient Access: There is no need for scientific internet access or complex network configuration. Simply deploy the DingoSpeed service and use it as the proxy address to easily complete the download.
* Traffic Reduction and Load Alleviation: Download once and use multiple times, reducing the traffic waste caused by repeated downloads, which is efficient and saves traffic.
* Localized Management: Cover the entire process of local compilation, deployment, monitoring, and usage of the mirror service, bringing an excellent and flexible experience. It avoids reliance on external networks and public mirror repositories, significantly improving the system's response speed and data security.

# Function List
1. [X] Implemented an HTTP RESTful API (compatible with the HF Hub specification) to support model and dataset downloads.
2. [X] Implemented multiple cache cleaning strategies (LRU/FIFO/LARGE_FIRST), scheduled tasks, and threshold triggers.
3. [X] Supports HTTP Range requests, enabling clients to resume interrupted downloads and allowing the server to download large files in chunks, reducing memory usage.
4. [X] Supports synchronizing cache data across multiple mirror nodes to avoid repeated downloads of the same file on multiple nodes.
5. [X] Supports storing large files in chunks and using multiple replica storage nodes.
6. [X] Low download time and high concurrent download success rate.
7. [X] Low memory usage.
8. [X] Stable download speed.


# System Architecture
![System Architecture](png/img.png)

# Installation
The project uses the wire command to generate the required dependency code. Install the wire command as follows：
```bash
# Import into the project
go get -u github.com/google/wire

# Install the command
go install github.com/google/wire/cmd/wire
```

Wire is a flexible dependency injection tool that completes dependency injection at compile time by automatically generating code. In the dependency relationships between various components, explicit initialization is usually used instead of passing global variables. Therefore, using Wire to initialize the code can effectively solve the coupling between components and improve code maintainability.
> This project uses go mod to manage dependencies and requires Go version 1.23 or higher. It uses makefile to manage the project and requires the make command.

```bash
# 1. Install dependencies
make init

# 2. Generate code
make wire

# 3. Compile the executable file for the current system version
make build

# 4. Compile the Linux executable file on macOS
make macbuild

# 5. Add a license to each file
make license

```
# Quick Start
Deploy the compiled binary file and execute ./dingospeed to start the service. Then set the environment variable HF_ENDPOINT to the mirror site (here it is http://localhost:8090/).

Linux:
```shell
export HF_ENDPOINT=http://localhost:8090
```
Windows Powershell:
```shell
$env:HF_ENDPOINT = "http://localhost:8090"
```
From now on, all download operations in the Hugging Face library will be proxied through this mirror site. You can install the Python library to try it out:

```shell
pip install -U huggingface_hub
```
```shell
from huggingface_hub import snapshot_download

snapshot_download(repo_id='Qwen/Qwen-7B', repo_type='model',
local_dir='./model_dir', resume_download=True,
max_workers=8)

```
Alternatively, you can use the Hugging Face CLI to directly download models and datasets.
Download GPT2:
```shell
huggingface-cli download --resume-download openai-community/gpt2 --local-dir gpt2
```
Download a single file:

```shell
huggingface-cli download --resume-download --force-download  HuggingFaceTB/SmolVLM-256M-Instruct config.json
```
Download WikiText:
```shell
huggingface-cli download --repo-type dataset --resume-download Salesforce/wikitext --local-dir wikitext
```
You can view the path ./repos, where the caches of all datasets and models are stored.

# Downloading Models
The file is divided into different segments of a certain size. The scheduling tool submits the tasks to the coroutine pool for execution. Each coroutine task submits the assigned length to the remote server for a request, reads the response results in chunks, and caches the results in the coroutine's exclusive work queue. The push coroutine then pushes the data to the client. At the same time, it checks whether the current chunk meets the size of a block. If it does, the block is written to the file.

![Downloading Models](png/img_download.png)

# Storing Models

The repository cache data file consists of a HEADER and data blocks. The functions of the HEADER are as follows:
1. Improve the readability of the cache file. Even if the configuration file is modified or the program is upgraded, it will not affect the reading of the cached file.
2. Efficiently check the existence of blocks without reading the actual database, improving operation efficiency.

![Storing Models](png/img_store.png)
