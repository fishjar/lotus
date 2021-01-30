# readme

```sh
#!/bin/bash

. ./env.sh


# green "写入配置文件: ${LOTUS_PATH}/config.toml"
# mkdir -p $LOTUS_PATH
# cat > ${LOTUS_PATH}/config.toml << EOF
# [API]
#     ListenAddress = "/ip4/${LOTUS_IP}/tcp/${LOTUS_PORT}/http"
#     RemoteListenAddress = "${LOTUS_IP}:${LOTUS_PORT}"
#     Timeout = "30s"
# EOF


## 下载 parameters （使用缓存文件不需下载）
# ./lotus fetch-params $PARAMS_SIEZ


#green "预密封扇区"
go run ./cmd/lotus-seed --sector-dir=$LOTUS_SECTOR_PATH pre-seal --sector-size $SECTOR_SIZE --num-sectors 2


#green "创建创世块"
go run ./cmd/lotus-seed genesis new $ROOT_PATH/localnet.json
go run ./cmd/lotus-seed genesis add-miner $ROOT_PATH/localnet.json $LOTUS_SECTOR_PATH/pre-seal-t01000.json


#green "启动守护程序"
go run ./cmd/lotus daemon \
    --api=$LOTUS_PORT \
    --lotus-make-genesis=$ROOT_PATH/devgen.car \
    --genesis-template=$ROOT_PATH/localnet.json \
    --bootstrap=false
```
