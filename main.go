package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/corona10/goimagehash"
)

// コンフィグ
type checkConfig struct {
	Ffmpeg      string `json:"ffmpeg"`
	Search      string `json:"search"`
	PhotoAccept int    `json:"photoAccept"`
	VideoAccept int    `json:"videoAccept"`
	QueueLimit  int    `json:"queueLimit"`
}

// カウンター
type FilesInfo struct {
	valueB, valueKB, valueMB, valueGB, valueTB int
	ImageFileCount                             int
	VideoFileCount                             int
	DirCount                                   int
}

// hash用struct
type photoHash struct {
	hash          *goimagehash.ImageHash
	path          string
	width, height int
}

type videoHash struct {
	hashs         [3]*goimagehash.ImageHash
	path          string
	width, height int
}

// json用struct
type JsonExport struct {
	Images []ImageInfo `json:"image,omitempty"`
	Videos []ImageInfo `json:"video,omitempty"`
}

type ImageInfo struct {
	Compare CompareImageData `json:"compare"`
	With    []WithImageData  `json:"with"`
	Time    int              `json:"time,omitempty"`
}

type CompareImageData struct {
	Path   string `json:"path"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type WithImageData struct {
	Path     string `json:"path"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Distance int    `json:"distance"`
}

var (
	configPath = flag.String("config", "./config.json", "Search Options Json File")
	config     = checkConfig{}
	photoFiles = []photoHash{}
	videoFiles = map[int][]videoHash{}
	info       = FilesInfo{}
	funcs      sync.WaitGroup // goroutienの終了待機用
	mapEdit    sync.Mutex
	result     = JsonExport{}
	errors     = []error{}
)

func init() {
	flag.Parse()
	configFile, _ := os.Open(*configPath)
	defer configFile.Close()
	configBytes, _ := io.ReadAll(configFile)
	json.Unmarshal(configBytes, &config)
}

func main() {
	fmt.Println("---[Config]---")
	fmt.Printf("ffmpeg      : %s\n", config.Ffmpeg)
	fmt.Printf("search      : %s\n", config.Search)
	fmt.Printf("photoAccept : %d\n", config.PhotoAccept)
	fmt.Printf("videoAccept : %d\n", config.VideoAccept)
	fmt.Printf("queueLimit  : %d\n\n", config.QueueLimit)
	time.Sleep(5 * time.Second)

	err := exec.Command(config.Ffmpeg, "-h").Run()
	if err != nil {
		fmt.Println(err)
		panic("Error: Failed Run ffmpeg!")
	}
	queue := make(chan struct{}, config.QueueLimit) // 並列上限
	filepath.WalkDir(config.Search, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Println(err)
			panic("Error: Failed Walk Directorys!")
		}
		// ディレクトリチェック
		index := strings.Count(path, string(os.PathSeparator))
		if d.IsDir() {
			fmt.Printf("%sDirectory: %s(%s)\n", Space(index-1), d.Name(), path)
			info.DirCount++
			fmt.Printf("%sNow Used: %dT %dG %dM %dK %dB %dDir %dFiles\n", Space(index-1), info.valueTB, info.valueGB, info.valueMB, info.valueKB, info.valueB, info.DirCount, info.ImageFileCount+info.VideoFileCount)
			return nil
		}
		// ファイルサイズ
		fileInfo, _ := os.Stat(path)
		ValueSize(fileInfo.Size())

		// 並列処理
		queue <- struct{}{} // queueの追加
		funcs.Add(1)        // 実行追加
		go func(pathFunc string, fileInfoFunc fs.FileInfo, infoFunc FilesInfo) {
			defer funcs.Done()
			defer func() { <-queue }()
			// ファイル処理
			fileExt := strings.ToLower(pathFunc)
			fileExt = filepath.Ext(fileExt)
			// ファイルの種類
			// 画像: jpeg jpg png webp jfif
			// 映像: mp4 mov webm (gif)

			if strings.Contains(".jpeg .jpg .png .webp .jfif", fileExt) {
				// ファイル数追加
				info.ImageFileCount++
				fmt.Printf("%s ・%-40s  (%s) Image No.%04d\n", Space(index), d.Name(), Size(fileInfoFunc.Size()), info.ImageFileCount)
				// 保存
				img, imgHash, err := image2Hash(pathFunc)
				if err != nil {
					errors = append(errors, err)
					return
				}
				photoFiles = append(photoFiles, photoHash{
					hash:   imgHash,
					path:   pathFunc,
					width:  img.Bounds().Dx(),
					height: img.Bounds().Dy(),
				})
				return
			}
			if strings.Contains(".mp4 .mov .webm", fileExt) {
				// ファイル数追加
				info.VideoFileCount++
				fmt.Printf("%s ・%-40s  (%s) Video No.%04d\n", Space(index), d.Name(), Size(fileInfoFunc.Size()), info.VideoFileCount)
				// 動画データ入手
				video := videoHash{
					path:  pathFunc,
					hashs: [3]*goimagehash.ImageHash{},
				}
				videoTime := 0
				out, _ := exec.Command(config.Ffmpeg, "-i", pathFunc).CombinedOutput()
				for _, line := range strings.Split(string(out), "\n") {
					// 動画時間入手
					if strings.Contains(line, "Duration") {
						line = regexp.MustCompile(".*([0-9]{2}):([0-9]{2}):([0-9]{2}).*").ReplaceAllString(line, "$1 $2 $3")
						var hour, min, sec int
						fmt.Sscanf(line, "%d %d %d", &hour, &min, &sec)
						videoTime = hour*3600 + min*60 + sec
					}
					// 動画の画質入手
					if strings.Contains(line, ": Video:") {
						line = regexp.MustCompile(".+?([0-9]{2,5})x([0-9]{2,5}).*").ReplaceAllString(line, "$1 $2")
						fmt.Sscanf(line, "%d %d", &video.width, &video.height)
					}
				}
				// 動画のスクショを入手
				videoPhotoTiming := videoTime / 3
				videoPhotoTimingOffset := videoPhotoTiming / 2
				for i := 0; i < 3; i++ {
					// dirチェック
					if _, err := os.Stat("./temp"); err != nil {
						err := os.Mkdir("./temp", 0666)
						if err != nil {
							panic(err)
						}
					}
					// 取り出し
					timing := fmt.Sprintf("%d", videoPhotoTiming-videoPhotoTimingOffset)
					tempPhoto := fmt.Sprintf("./temp/%s_videoPhoto.png", fileInfoFunc.Name())
					exec.Command(config.Ffmpeg, "-ss", timing, "-i", pathFunc, "-frames:v", "1", tempPhoto).Run()
					// Hash保存
					_, hash, err := image2Hash(fmt.Sprintf("./temp/%s_videoPhoto.png", fileInfoFunc.Name()))
					if err != nil {
						errors = append(errors, err)
						return
					}
					video.hashs[i] = hash
				}
				// temp写真削除
				os.Remove(fmt.Sprintf("./temp/%s_videoPhoto.png", fileInfoFunc.Name()))
				// 保存
				mapEdit.Lock()
				defer mapEdit.Unlock()
				videoFiles[videoTime] = append(videoFiles[videoTime], video)
				return
			}
			fmt.Printf("%s ・%-40s  (%s)                Skip(%s)\n", Space(index), d.Name(), Size(fileInfoFunc.Size()), fileExt)
		}(path, fileInfo, info)
		return nil
	})
	funcs.Wait()

	fmt.Println("")
	fmt.Printf("Now Used: %dT %dG %dM %dK %dB %dDir %dFiles\n", info.valueTB, info.valueGB, info.valueMB, info.valueKB, info.valueB, info.DirCount, info.ImageFileCount+info.VideoFileCount)
	fmt.Println("")
	fmt.Println("----------------------------------------")
	fmt.Println("")
	fmt.Println("File Scan End, Will Be Start Check")
	fmt.Println("")
	fmt.Println("----------------------------------------")
	fmt.Println("")

	// 画像
	for i := 0; i < len(photoFiles); i++ {
		data1 := photoFiles[i]
		var duplicates []WithImageData
		for j := i + 1; j < len(photoFiles); j++ {
			data2 := photoFiles[j]
			distance, _ := data1.hash.Distance(data2.hash)
			if distance <= config.PhotoAccept {
				// json用に保存
				duplicates = append(duplicates, WithImageData{
					Path:     data2.path,
					Width:    data2.width,
					Height:   data2.height,
					Distance: distance,
				})
			}
			// 引っかかったのは今後検索に掛けない
			photoFiles = append(photoFiles[:j], photoFiles[j+1:]...)
		}
		// 重複があればjsonに保存
		if len(duplicates) > 0 {
			result.Images = append(result.Images, ImageInfo{
				Compare: CompareImageData{
					Path:   data1.path,
					Width:  data1.width,
					Height: data1.height,
				},
				With: duplicates,
			})
			// 見やすく表示
			fmt.Printf("Compare: [%4dpx*%4dpx] %s \n", data1.width, data1.height, data1.path)
			for j := 0; j < len(duplicates); j++ {
				fmt.Printf("         [%4dpx*%4dpx] Distance:%-3d %s\n", duplicates[j].Width, duplicates[j].Height, duplicates[j].Distance, duplicates[j].Path)
			}
		}
	}
	// 動画
	for videoTime, videos := range videoFiles {
		if len(videos) < 2 {
			continue
		}
		for i := 0; i < len(videos); i++ {
			data1 := videos[i]
			var duplicates []WithImageData
			for j := i + 1; j < len(videos); j++ {
				data2 := videos[j]
				distance := 0
				for k := 0; k < 3; k++ {
					imageDistance, _ := data1.hashs[k].Distance(data2.hashs[k])
					distance += imageDistance
				}
				if distance <= config.VideoAccept {
					// json用に保存
					duplicates = append(duplicates, WithImageData{
						Path:     data2.path,
						Width:    data2.width,
						Height:   data2.height,
						Distance: distance,
					})
				}
				// 引っかかったのは今後検索に掛けない
				videos = append(videos[:j], videos[j+1:]...)
			}
			// 重複があればjsonに保存
			if len(duplicates) > 0 {
				result.Images = append(result.Images, ImageInfo{
					Compare: CompareImageData{
						Path:   data1.path,
						Width:  data1.width,
						Height: data1.height,
					},
					With: duplicates,
					Time: videoTime,
				})
				// 見やすく表示
				fmt.Printf("Compare: [%4dpx*%4dpx] %s \n", data1.width, data1.height, data1.path)
				for j := 0; j < len(duplicates); j++ {
					fmt.Printf("         [%4dpx*%4dpx] Distance:%-3d %s\n", duplicates[j].Width, duplicates[j].Height, duplicates[j].Distance, duplicates[j].Path)
				}
			}
		}
	}
	fmt.Println("")
	fmt.Println("----------------------------------------")
	fmt.Println("")
	fmt.Println("File Check End, Saving To Json")
	fmt.Println("")
	fmt.Println("----------------------------------------")
	fmt.Println("")

	if _, err := os.Stat("./duplicate.json"); err != nil {
		_, err = os.Create("./duplicate.json")
		if err != nil {
			panic(err)
		}
	}
	resultBytes, _ := json.MarshalIndent(result, "", "  ")
	ioutil.WriteFile("./duplicate.json", resultBytes, 0644)

	fmt.Println("")
	fmt.Println("----------------------------------------")
	fmt.Println("")
	fmt.Println("Save End")
	fmt.Println("")
	fmt.Println("----------------------------------------")
	fmt.Println("")

	// エラー表示
	if len(errors) > 0 {
		fmt.Println("Errors")
		for _, v := range errors {
			fmt.Println(v.Error())
		}
	}
}

func Space(n int) (spacer string) {
	for i := 0; i < n; i++ {
		spacer += "  "
	}
	return
}

func Size(n int64) (size string) {
	// 接頭辞表記
	x := float32(n)
	if x < 1000 {
		return fmt.Sprintf("%7.2f B", x)
	} else if x < 1000*1000 {
		return fmt.Sprintf("%7.2fKB", x/1000)
	} else if x < 1000*1000*1000 {
		return fmt.Sprintf("%7.2fMB", x/1000/1000)
	} else if x < 1000*1000*1000*1000 {
		return fmt.Sprintf("%7.2fGB", x/1000/1000/1000)
	}
	return fmt.Sprintf("%7.2fTB", x/1000/1000/1000/1000)
}

func ValueSize(n int64) {
	info.valueB += int(n)
	if info.valueB >= 1000 {
		info.valueKB += info.valueB / 1000
		info.valueB = info.valueB % 1000
	}
	if info.valueKB >= 1000 {
		info.valueMB += info.valueKB / 1000
		info.valueKB = info.valueKB % 1000
	}
	if info.valueMB >= 1000 {
		info.valueGB += info.valueMB / 1000
		info.valueMB = info.valueMB % 1000
	}
	if info.valueGB >= 1000 {
		info.valueTB += info.valueGB / 1000
		info.valueGB = info.valueGB % 1000
	}
}

func image2Hash(file string) (img image.Image, imgHash *goimagehash.ImageHash, err error) {
	var imgFile *os.File
	imgFile, err = os.Open(file)
	if err != nil {
		return
	}
	defer imgFile.Close()
	img, _, err = image.Decode(imgFile)
	if err != nil {
		return
	}
	imgHash, _ = goimagehash.PerceptionHash(img)
	return
}
