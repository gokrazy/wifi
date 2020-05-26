package main

//go:generate sh -c "go run goembed.go -package main -var firmware third_party/firmware-nonfree/brcmfmac43455-sdio.bin third_party/firmware-nonfree/brcmfmac43455-sdio.txt third_party/firmware-nonfree/brcmfmac43455-sdio.clm_blob > GENERATED_firmware.go && gofmt -w GENERATED_firmware.go"
