# dingo-hfmirror
dingo-hfmirror is a self-hosted lightweight huggingface mirror service

# install

## 快速开始
### 安装依赖
项目会使用wire命令生成所需的依赖代码，安装wire命令如下：
```bash
# 导入到项目中
go get -u github.com/google/wire

# 安装命令
go install github.com/google/wire/cmd/wire
```

Wire 是一个灵活的依赖注入工具，通过自动生成代码的方式在编译期完成依赖注入。 在各个组件之间的依赖关系中，通常显式初始化，而不是全局变量传递。 所以通过 Wire 进行初始化代码，可以很好地解决组件之间的耦合，以及提高代码维护性。

> 本项目使用go mod管理依赖，需要go1.23以上版本。使用makefile管理项目，需要make命令

```bash
# 1. 安装依赖
make init

# 2. 代码生成
make wire

# 3. 编译可执行文件
make build
```
