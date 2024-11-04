# imageDuplicate
Golangで自分のために作成  
開発環境:rpi4(8gb) golang(go1.17.8 linux/arm64) ffmpeg(4.1.8-0+deb10u1+rpt1)  

## -起動-  
```go run main.go -config=config.json```
check similar files
```go run main.go -port=8080```
preview similar files

## json解説  
`duplicate.json`  
distance: 類似度  
time: 動画の時間(sec)  
path,width,height: そのまんま  
  
`config.json`
ffmpeg: ffmpegへのPath(動画がある場合は必須)  
search: 検索するディレクトリ  
photoAccept: 許容される画像の類似度の最大数値(初期値:5)  
videoAccept: 許容される動画の類似度の最大数値(初期値:15)  
queueLimit: 並列処理数(初期値:30)  
  
## コード元:  
Bot Language   : https://golang.org/  
