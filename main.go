package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/corona10/goimagehash"
)

// コンフィグ
type checkConfig struct {
	Ffmpeg      string   `json:"ffmpeg"`
	Search      []string `json:"search"`
	PhotoAccept int      `json:"photoAccept"`
	VideoAccept int      `json:"videoAccept"`
	QueueLimit  int      `json:"queueLimit"`
}

type hashInfo struct {
	hash   *goimagehash.ImageHash
	width  int
	height int
}

// カウンター
type dataInfo struct {
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
	hash          [3]*goimagehash.ImageHash
	path          string
	width, height int
	time          int
}

// json用struct
type JsonExport struct {
	Similar []SimilarFile `json:"similar"`
	Unique  []SourceImage `json:"unique"`
}

type SimilarFile struct {
	Source SourceImage   `json:"source"`
	With   []SimilarInfo `json:"with"`
}

type SourceImage struct {
	Path   string `json:"path"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type SimilarInfo struct {
	Dest     SourceImage `json:"dest"`
	Distance int         `json:"distance"`
}

var (
	configPath = flag.String("config", "./config.json", "Search Options Json File")
	port       = flag.Int("port", 0, "Boot Web Server Port")
	config     = checkConfig{}
	photoFiles = []photoHash{}
	videoFiles = []videoHash{}
	data       = dataInfo{}
	funcs      sync.WaitGroup // go routineの終了待機用
	mapEdit    sync.Mutex
	result     = JsonExport{}
	errors     = []error{}
)

func main() {
	// Parse flag
	flag.Parse()
	if *port > 0 {
		_, file, _, _ := runtime.Caller(0)
		os.Chdir(filepath.Dir(file) + "/")

		// アクセス先
		http.HandleFunc("/", HttpResponse)
		// Web鯖 起動
		go func() {
			log.Println("Http Server Boot")
			err := http.ListenAndServe(fmt.Sprintf("0.0.0.0:%d", *port), nil)
			if err != nil {
				log.Println("Failed Listen:", err)
				return
			}
		}()

		sc := make(chan os.Signal, 1)
		signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
		<-sc
		return
	}

	configFile, _ := os.Open(*configPath)
	defer configFile.Close()
	configBytes, _ := io.ReadAll(configFile)
	json.Unmarshal(configBytes, &config)

	// コンフィグ表示
	fmt.Println("-----[Config]-----")
	fmt.Printf("ffmpeg      : %s\n", config.Ffmpeg)
	fmt.Printf("search      : %s\n", config.Search)
	fmt.Printf("photoAccept : %d\n", config.PhotoAccept)
	fmt.Printf("videoAccept : %d\n", config.VideoAccept)
	fmt.Printf("queueLimit  : %d\n", config.QueueLimit)
	time.Sleep(5 * time.Second)

	// ffmpegチェック
	err := exec.Command(config.Ffmpeg, "-h").Run()
	if err != nil {
		fmt.Println(err)
		panic("Error: Failed Run ffmpeg!")
	}

	// search
	fmt.Println("")
	fmt.Println("----------------------------------------")
	fmt.Println("Start File Scan")
	fmt.Println("----------------------------------------")
	fmt.Println("")
	queue := make(chan struct{}, config.QueueLimit) // 並列上限
	for _, searchDir := range config.Search {
		fmt.Printf("  Search: %s", searchDir)
		filepath.WalkDir(searchDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				fmt.Println(err)
				panic("Error: Failed Walk Directory!")
			}

			// ディレクトリチェック
			index := strings.Count(path, string(os.PathSeparator))
			if d.IsDir() {
				fmt.Printf("%sDirectory: %s(%s)\n", strings.Repeat(" ", index-1), d.Name(), path)
				data.DirCount++
				fmt.Printf("%sNow Used: %dT %dG %dM %dK %dB %dDir %dFiles\n", strings.Repeat(" ", index-1), data.valueTB, data.valueGB, data.valueMB, data.valueKB, data.valueB, data.DirCount, data.ImageFileCount+data.VideoFileCount)
				return nil
			}
			// ファイルサイズ
			fileInfo, _ := os.Stat(path)
			ValueSize(fileInfo.Size())

			// 並列処理
			queue <- struct{}{} // queueの追加
			funcs.Add(1)        // 実行追加
			go func(pathFunc string, fileInfoFunc fs.FileInfo) {
				defer funcs.Done()
				defer func() { <-queue }()
				// ファイル処理
				fileExt := strings.ToLower(pathFunc)
				fileExt = filepath.Ext(fileExt)

				// 画像(jpeg jpg png webp jfif) の処理
				if strings.Contains(".jpeg .jpg .png .webp .jfif", fileExt) {
					// ファイル数追加
					data.ImageFileCount++
					fmt.Printf("%s ・%-40s  (%s) Image No.%04d\n", strings.Repeat(" ", index), d.Name(), Size(fileInfoFunc.Size()), data.ImageFileCount)

					info, err := getImageHash(pathFunc)
					if err != nil {
						errors = append(errors, err)
						return
					}

					// 保存
					mapEdit.Lock()
					defer mapEdit.Unlock()
					photoFiles = append(photoFiles, photoHash{
						hash:   info.hash,
						path:   pathFunc,
						width:  info.width,
						height: info.height,
					})

					return
				}

				// 映像(mp4 mov webm) の処理
				if strings.Contains(".mp4 .mov .webm", fileExt) {
					// ファイル数追加
					data.VideoFileCount++
					fmt.Printf("%s ・%-40s  (%s) Video No.%04d\n", strings.Repeat(" ", index), d.Name(), Size(fileInfoFunc.Size()), data.VideoFileCount)
					// 動画データ入手
					hashList := [3]*goimagehash.ImageHash{}

					duration := getVideoInfo(pathFunc)
					// 動画のスクショを入手
					videoPhotoTiming := duration / 3
					videoPhotoTimingOffset := videoPhotoTiming / 2
					for i := 0; i < 3; i++ {
						// dirチェック
						if _, err := os.Stat("./temp"); err != nil {
							err := os.Mkdir("./temp", 0755)
							if err != nil {
								panic(err)
							}
						}
						// 取り出し
						timing := fmt.Sprintf("%d", videoPhotoTiming*i-videoPhotoTimingOffset)
						tempPhoto := fmt.Sprintf("./temp/%s_videoPhoto.png", fileInfoFunc.Name())
						exec.Command(config.Ffmpeg, "-ss", timing, "-i", pathFunc, "-frames:v", "1", tempPhoto).Run()
						// Hash保存
						info, err := getImageHash(fmt.Sprintf("./temp/%s_videoPhoto.png", fileInfoFunc.Name()))
						if err != nil {
							errors = append(errors, err)
							return
						}
						hashList[i] = info.hash

						if i == 2 {
							// 保存
							mapEdit.Lock()
							defer mapEdit.Unlock()
							videoFiles = append(videoFiles, videoHash{
								hash:   hashList,
								path:   pathFunc,
								width:  info.width,
								height: info.height,
								time:   duration,
							})
						}
					}
					// temp写真削除
					os.Remove(fmt.Sprintf("./temp/%s_videoPhoto.png", fileInfoFunc.Name()))

					return
				}
				fmt.Printf("%s ・%-40s  (%s)                Skip(%s)\n", strings.Repeat(" ", index), d.Name(), Size(fileInfoFunc.Size()), fileExt)
			}(path, fileInfo)
			return nil
		})
	}
	funcs.Wait()

	fmt.Println("")
	fmt.Printf("Scanned File: %dT %dG %dM %dK %dB %dDir %dFiles(%dPhoto, %dVideo)\n", data.valueTB, data.valueGB, data.valueMB, data.valueKB, data.valueB, data.DirCount, data.ImageFileCount+data.VideoFileCount, len(photoFiles), len(videoFiles))
	fmt.Println("----------------------------------------")
	fmt.Println("File Scan End")
	fmt.Println("----------------------------------------")
	fmt.Println("")
	fmt.Println("----------------------------------------")
	fmt.Println("Check Start Duplicate/Similar Files")
	fmt.Println("----------------------------------------")
	fmt.Println("")

	// 類似チェック 画像
	fmt.Println("-----[Check Similar Photo]-----")
	for i := 0; i < len(photoFiles); i++ {
		src := photoFiles[i]
		var similar []SimilarInfo
		for j := i + 1; j < len(photoFiles); j++ {
			dest := photoFiles[j]
			// Path Check
			if src.path == dest.path {
				continue
			}
			distance, _ := src.hash.Distance(dest.hash)
			if distance <= config.PhotoAccept {
				// json用に保存
				similar = append(similar, SimilarInfo{
					Dest: SourceImage{
						Path:   dest.path,
						Width:  dest.width,
						Height: dest.height,
					},
					Distance: distance,
				})
				// 引っかかったのは今後検索に掛けない
				photoFiles = append(photoFiles[:j], photoFiles[j+1:]...)
			}
		}
		// 重複があればjsonに保存
		if len(similar) > 0 {
			// ソート
			sort.SliceStable(similar, func(i, j int) bool {
				return similar[i].Distance > similar[j].Distance
			})

			// 追記
			result.Similar = append(result.Similar, SimilarFile{
				Source: SourceImage{
					Path:   src.path,
					Width:  src.width,
					Height: src.height,
				},
				With: similar,
			})
			// 見やすく表示
			fmt.Printf("    Similar\n")
			fmt.Printf("        Compare: [%4dpx*%4dpx] %s \n", src.width, src.height, src.path)
			for j := 0; j < len(similar); j++ {
				fmt.Printf("            [%4dpx*%4dpx] Distance:%-3d %s\n", similar[j].Dest.Width, similar[j].Dest.Height, similar[j].Distance, similar[j].Dest.Path)
			}
			photoFiles = append(photoFiles[:i], photoFiles[i+1:]...)
		}
	}
	// 類似チェック 動画
	fmt.Println("-----[Check Similar Video]-----")
	for i := 0; i < len(videoFiles); i++ {
		src := videoFiles[i]
		var similar []SimilarInfo
		for j := i + 1; j < len(videoFiles); j++ {
			dest := videoFiles[j]
			// Path Check
			if src.path == dest.path {
				continue
			}
			// Time Check
			if src.time != dest.time {
				continue
			}

			distance := 0
			for k := 0; k < 3; k++ {
				imageDistance, _ := src.hash[k].Distance(dest.hash[k])
				distance += imageDistance
			}
			if distance <= config.VideoAccept {
				// json用に保存
				similar = append(similar, SimilarInfo{
					Dest: SourceImage{
						Path:   dest.path,
						Width:  dest.width,
						Height: dest.height,
					},
					Distance: distance,
				})
				// 引っかかったのは今後検索に掛けない
				videoFiles = append(videoFiles[:j], videoFiles[j+1:]...)
			}
		}
		// 重複があればjsonに保存
		if len(similar) > 0 {
			// ソート
			sort.SliceStable(similar, func(i, j int) bool {
				return similar[i].Distance > similar[j].Distance
			})

			// 追記
			result.Similar = append(result.Similar, SimilarFile{
				Source: SourceImage{
					Path:   src.path,
					Width:  src.width,
					Height: src.height,
				},
				With: similar,
			})
			// 見やすく表示
			fmt.Printf("    Similar\n")
			fmt.Printf("        Compare: [%4dpx*%4dpx] %s \n", src.width, src.height, src.path)
			for j := 0; j < len(similar); j++ {
				fmt.Printf("            [%4dpx*%4dpx] Distance:%-3d %s\n", similar[j].Dest.Width, similar[j].Dest.Height, similar[j].Distance, similar[j].Dest.Path)
			}
		}
		videoFiles = append(videoFiles[:i], videoFiles[i+1:]...)
	}

	// Hashを保存
	for i := 0; i < len(photoFiles); i++ {
		photo := photoFiles[i]
		result.Unique = append(result.Unique, SourceImage{
			Path:   photo.path,
			Width:  photo.width,
			Height: photo.height,
		})
	}
	for i := 0; i < len(videoFiles); i++ {
		video := videoFiles[i]
		result.Unique = append(result.Unique, SourceImage{
			Path:   video.path,
			Width:  video.width,
			Height: video.height,
		})
	}
	fmt.Println("")
	fmt.Println("----------------------------------------")
	fmt.Println("Check End Duplicate/Similar Files")
	fmt.Println("----------------------------------------")
	fmt.Println("")
	fmt.Println("----------------------------------------")
	fmt.Println("Save Start Duplicate/Similar Data To Json")
	fmt.Println("----------------------------------------")
	fmt.Println("")

	fmt.Println("-----[Data Formatting Start (To Json)]-----")
	resultBytes, _ := json.MarshalIndent(result, "", "  ")

	fmt.Println("-----[Data Formatting End]-----")
	fmt.Println("-----[Writing Start To \"duplicate.json\"]-----")
	// 書き込み
	jsonFile, _ := os.Create("./duplicate.json")
	defer jsonFile.Close()
	writer := bufio.NewWriter(jsonFile)
	writer.Write(resultBytes)
	writer.Flush()
	fmt.Println("-----[Writing End To \"duplicate.json\"]-----")

	fmt.Println("")
	fmt.Println("----------------------------------------")
	fmt.Println("Save End Duplicate/Similar Data To Json")
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

func HttpResponse(w http.ResponseWriter, r *http.Request) {
	log.Println("Access:", r.RemoteAddr, r.RequestURI)
	file := r.URL.Query().Get("file")
	var bytes = []byte{}
	if file == "json" {
		bytes, _ = os.ReadFile("./duplicate.json")
	} else if file != "" {
		bytes, _ = os.ReadFile(file)
	} else {
		bytes, _ = os.ReadFile("./index.html")
	}
	w.Write(bytes)
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
	data.valueB += int(n)
	if data.valueB >= 1000 {
		data.valueKB += data.valueB / 1000
		data.valueB = data.valueB % 1000
	}
	if data.valueKB >= 1000 {
		data.valueMB += data.valueKB / 1000
		data.valueKB = data.valueKB % 1000
	}
	if data.valueMB >= 1000 {
		data.valueGB += data.valueMB / 1000
		data.valueMB = data.valueMB % 1000
	}
	if data.valueGB >= 1000 {
		data.valueTB += data.valueGB / 1000
		data.valueGB = data.valueGB % 1000
	}
}

func getImageHash(file string) (info hashInfo, err error) {
	f, err := os.Open(file)
	if err != nil {
		return
	}
	defer f.Close()
	// ImageHash化
	img, _, err := image.Decode(f)
	if err != nil {
		return
	}
	imgHash, _ := goimagehash.PerceptionHash(img)

	info = hashInfo{
		hash:   imgHash,
		width:  img.Bounds().Dx(),
		height: img.Bounds().Dy(),
	}
	return
}

func getVideoInfo(file string) (duration int) {
	out, _ := exec.Command(config.Ffmpeg, "-i", file).CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		// 動画時間入手
		if strings.Contains(line, "Duration") {
			line = regexp.MustCompile(".*([0-9]{2}):([0-9]{2}):([0-9]{2}).*").ReplaceAllString(line, "$1 $2 $3")
			var hour, min, sec int
			fmt.Sscanf(line, "%d %d %d", &hour, &min, &sec)
			duration = hour*3600 + min*60 + sec
		}
	}
	return
}
