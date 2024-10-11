package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eiannone/keyboard"
	"github.com/go-audio/aiff"
	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
	"github.com/go-vgo/robotgo"
	"github.com/gordonklaus/portaudio"
	"github.com/joho/godotenv"
)

const openAiUrlTranscriptions = "https://api.openai.com/v1/audio/transcriptions"

var (
	openAiApiKey string
	envName      string
)

func main() {
	var dirPath string

	switch len(os.Args) {
	case 1:
		dirPath = "recordings"
	case 2:
		dirPath = os.Args[1]
	default:
		fmt.Println("Too many arguments. Usage: <program> [output directory path]")
		os.Exit(1)
	}
	// Get ENV_NAME or set a default value if not present
	envName = os.Getenv("ENV_NAME")
	if envName == "" {
		envName = "LOCAL"
	}

	// Load environment variables from .env file
	err := godotenv.Load(fmt.Sprintf("%s.env", strings.ToLower(envName)))
	if err != nil {
		log.Fatal("Error loading .env file", err)
	}

	// Use environment variables
	openAiApiKey = os.Getenv("OPENAI_API_KEY")

	aiffFileName := filepath.Join(dirPath, "quartzy.aiff")
	wavFileName := filepath.Join(dirPath, "quartzy.wav")

	recordToAIFF(aiffFileName)
	convertToWAV(aiffFileName, wavFileName)

	fmt.Println("Recording and conversion completed.")

	// Upload WAV to OpenAI transcription, returning text
	text := openAiTranscription(wavFileName)

	// Simulate keyboard input with transcription result
	simulateKeyboardInput(text)
}

func recordToAIFF(fileName string) {
	// List available devices
	portaudio.Initialize()
	defer portaudio.Terminate()
	devices, err := portaudio.Devices()
	chk(err)

	fmt.Println("Available devices:")
	for i, device := range devices {
		fmt.Printf("%d: %s\n", i, device.Name)
	}

	// Select device
	var deviceIndex int
	fmt.Print("Select device index: ")
	_, err = fmt.Scanf("%d", &deviceIndex)
	chk(err)

	if deviceIndex < 0 || deviceIndex >= len(devices) {
		fmt.Println("Invalid device index")
		return
	}

	selectedDevice := devices[deviceIndex]

	// Initialize keyboard listener
	if err := keyboard.Open(); err != nil {
		fmt.Println("Error initializing keyboard listener:", err)
		return
	}
	defer keyboard.Close()

	sig := make(chan struct{}, 1)
	go func() {
		for {
			_, key, err := keyboard.GetKey()
			if err != nil {
				fmt.Println("Error reading key:", err)
				return
			}
			if key == keyboard.KeyCtrlR {
				close(sig)
				return
			}
		}
	}()

	fmt.Println("Recording... Press Ctrl-R to stop.")

	f, err := os.Create(fileName)
	if err != nil {
		fmt.Printf("Error creating file %s: %v\n", fileName, err)
		return
	}
	chk(err)

	// form chunk
	_, err = f.WriteString("FORM")
	chk(err)
	chk(binary.Write(f, binary.BigEndian, int32(0))) // total bytes
	_, err = f.WriteString("AIFF")
	chk(err)

	// common chunk
	_, err = f.WriteString("COMM")
	chk(err)
	chk(binary.Write(f, binary.BigEndian, int32(18)))                  // size
	chk(binary.Write(f, binary.BigEndian, int16(1)))                   // channels
	chk(binary.Write(f, binary.BigEndian, int32(0)))                   // number of samples
	chk(binary.Write(f, binary.BigEndian, int16(32)))                  // bits per sample
	_, err = f.Write([]byte{0x40, 0x0e, 0xac, 0x44, 0, 0, 0, 0, 0, 0}) // 80-bit sample rate 44100
	chk(err)

	// sound chunk
	_, err = f.WriteString("SSND")
	chk(err)
	chk(binary.Write(f, binary.BigEndian, int32(0))) // size
	chk(binary.Write(f, binary.BigEndian, int32(0))) // offset
	chk(binary.Write(f, binary.BigEndian, int32(0))) // block
	nSamples := 0
	defer func() {
		// fill in missing sizes
		totalBytes := 4 + 8 + 18 + 8 + 4*nSamples
		_, err = f.Seek(4, 0)
		chk(err)
		chk(binary.Write(f, binary.BigEndian, int32(totalBytes)))
		_, err = f.Seek(22, 0)
		chk(err)
		chk(binary.Write(f, binary.BigEndian, int32(nSamples)))
		_, err = f.Seek(42, 0)
		chk(err)
		chk(binary.Write(f, binary.BigEndian, int32(4*nSamples+8)))
		chk(f.Close())
	}()

	in := make([]int32, 64)
	stream, err := portaudio.OpenStream(portaudio.StreamParameters{
		Input: portaudio.StreamDeviceParameters{
			Device:   selectedDevice,
			Channels: 1,
			Latency:  selectedDevice.DefaultLowInputLatency,
		},
		SampleRate:      44100,
		FramesPerBuffer: len(in),
	}, in)
	chk(err)
	defer stream.Close()

	chk(stream.Start())
	for {
		chk(stream.Read())
		chk(binary.Write(f, binary.BigEndian, in))
		nSamples += len(in)
		select {
		case <-sig:
			chk(stream.Stop())
			fmt.Println("Recording stopped.")
			return
		default:
		}
	}
}

func convertToWAV(aiffFileName, wavFileName string) {
	err := transformAIFFToWAV(aiffFileName, wavFileName)
	if err != nil {
		fmt.Println("Error converting AIFF to WAV:", err)
	}
}

func transformAIFFToWAV(sourcePath, outPath string) error {
	f, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("invalid path %s: %v", sourcePath, err)
	}
	defer f.Close()

	d := aiff.NewDecoder(f)
	if !d.IsValidFile() {
		return fmt.Errorf("invalid AIFF file")
	}

	of, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("failed to create %s: %v", outPath, err)
	}
	defer of.Close()

	e := wav.NewEncoder(of, int(d.SampleRate), int(d.BitDepth), int(d.NumChans), 1)

	format := &audio.Format{
		NumChannels: int(d.NumChans),
		SampleRate:  int(d.SampleRate),
	}

	bufferSize := 1000000
	buf := &audio.IntBuffer{Data: make([]int, bufferSize), Format: format}
	var n int
	for err == nil {
		n, err = d.PCMBuffer(buf)
		if err != nil {
			break
		}
		if n == 0 {
			break
		}
		if n != len(buf.Data) {
			buf.Data = buf.Data[:n]
		}
		if err := e.Write(buf); err != nil {
			return fmt.Errorf("error writing buffer: %v", err)
		}
	}

	if err := e.Close(); err != nil {
		return fmt.Errorf("error closing encoder: %v", err)
	}
	fmt.Printf("AIFF file converted to %s\n", outPath)
	return nil
}

func chk(err error) {
	if err != nil {
		panic(err)
	}
}

func openAiTranscription(wavFileName string) string {
	log.Println("Opening WAV file for transcription...")
	file, err := os.Open(wavFileName)
	if err != nil {
		log.Println("Error opening WAV file:", err)
		return ""
	}
	defer file.Close()

	log.Println("Creating multipart form data...")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", wavFileName)
	if err != nil {
		log.Println("Error creating form file:", err)
		return ""
	}

	log.Println("Copying file content to form...")
	_, err = io.Copy(part, file)
	if err != nil {
		log.Println("Error copying file content:", err)
		return ""
	}

	writer.WriteField("model", "whisper-1")
	writer.Close()

	log.Println("Creating HTTP request...")
	req, err := http.NewRequest("POST", openAiUrlTranscriptions, body)
	if err != nil {
		log.Println("Error creating request:", err)
		return ""
	}

	req.Header.Set("Authorization", "Bearer "+openAiApiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	log.Println("Sending HTTP request...")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Println("Error sending request:", err)
		return ""
	}
	defer resp.Body.Close()

	log.Println("Request sent. Parsing response...")
	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Println("Error decoding response:", err)
		return ""
	}

	log.Println("Transcription received:", result.Text)
	return result.Text
}

func simulateKeyboardInput(text string) {
	const delaySeconds = 5
	fmt.Printf("Typing will start in %d seconds...\n", delaySeconds)
	time.Sleep(delaySeconds * time.Second) // Add a 3-second delay

	for _, char := range text {
		robotgo.TypeStrDelay(string(char), 25)
	}

	fmt.Println("\nTyping completed.")
}
