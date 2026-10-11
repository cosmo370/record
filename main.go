package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

type transcriptionResponse struct {
	Text string `json:"text"`
}

func makeTempPath() string {
	if err := os.MkdirAll("recordings", 0o755); err != nil {
		panic(err)
	}
	return filepath.Join("recordings", time.Now().Format("20060102-150405")+".tmp.mp4")
}

func finalizePath(tempPath string) string {
	finalPath := strings.TrimSuffix(tempPath, ".tmp.mp4") + ".mp4"
	if err := os.Rename(tempPath, finalPath); err != nil {
		return tempPath
	}
	return finalPath
}

func webcamInputArgs() []string {
	if _, err := os.Stat("/dev/video0"); err == nil {
		return []string{"-f", "v4l2", "-i", "/dev/video0"}
	}
	return []string{"-f", "lavfi", "-i", "testsrc=size=1280x720:rate=30"}
}

func startFFmpegRecording(outputPath string) (*exec.Cmd, error) {
	args := append([]string{"ffmpeg"}, webcamInputArgs()...)
	args = append(args,
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-r", "30",
		"-y",
		outputPath,
	)

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func stopFFmpegRecording(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		return err
	}

	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 255 {
			return nil
		}
		return err
	}
	return nil
}

func extractAudioForSTT(videoPath string) (string, error) {
	audioPath := strings.TrimSuffix(videoPath, filepath.Ext(videoPath)) + ".wav"
	cmd := exec.Command(
		"ffmpeg",
		"-y",
		"-i", videoPath,
		"-vn",
		"-acodec", "pcm_s16le",
		"-ar", "16000",
		"-ac", "1",
		audioPath,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return audioPath, nil
}

func transcribeAudioWithOpenRouter(audioPath string) (string, error) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		return "", errors.New("OPENROUTER_API_KEY")
	}

	file, err := os.Open(audioPath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return "", err
	}
	if _, err = io.Copy(part, file); err != nil {
		return "", err
	}
	if err = writer.WriteField("model", "x-ai/grok-stt-1.0"); err != nil {
		return "", err
	}
	if err = writer.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", "https://openrouter.ai/api/v1/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("HTTP-Referer", "http://localhost")
	req.Header.Set("X-Title", "Recorder")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("OpenRouter API error %s: %s", resp.Status, string(respData))
	}

	var result transcriptionResponse
	if err := json.Unmarshal(respData, &result); err != nil {
		return "", err
	}
	if strings.TrimSpace(result.Text) == "" {
		return "", errors.New("transcription result was empty")
	}
	return result.Text, nil
}

func buildSRTFromText(text string) string {
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return ""
	}

	const chunkSeconds = 5
	chunks := make([]string, 0)
	for i := 0; i < len(parts); i += 12 {
		end := i + 12
		if end > len(parts) {
			end = len(parts)
		}
		chunks = append(chunks, strings.Join(parts[i:end], " "))
	}

	var b strings.Builder
	for i, chunk := range chunks {
		start := time.Duration(i*chunkSeconds) * time.Second
		end := time.Duration((i+1)*chunkSeconds) * time.Second
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, formatSRTTime(start), formatSRTTime(end), chunk)
	}
	return b.String()
}

func formatSRTTime(d time.Duration) string {
	totalMs := d / time.Millisecond
	hours := int(totalMs / (60 * 60 * 1000))
	minutes := int((totalMs % (60 * 60 * 1000)) / (60 * 1000))
	seconds := int((totalMs % (60 * 1000)) / 1000)
	millis := int(totalMs % 1000)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", hours, minutes, seconds, millis)
}

func makeSRTFromVideo(videoPath string) (string, error) {
	audioPath, err := extractAudioForSTT(videoPath)
	if err != nil {
		return "", err
	}
	defer os.Remove(audioPath)

	text, err := transcribeAudioWithOpenRouter(audioPath)
	if err != nil {
		return "", err
	}

	srtPath := strings.TrimSuffix(videoPath, filepath.Ext(videoPath)) + ".srt"
	srtText := buildSRTFromText(text)
	if err := os.WriteFile(srtPath, []byte(srtText), 0o644); err != nil {
		return "", err
	}
	return srtPath, nil
}

func main() {
	a := app.New()
	w := a.NewWindow("Record App")
	w.Resize(fyne.NewSize(640, 480))

	status := widget.NewLabel("Ready")
	lastAction := widget.NewLabel("No recordings yet")

	var isRecording bool
	var currentCmd *exec.Cmd
	var outputPath string

	startBtn := widget.NewButton("Start Recording", func() {
		if isRecording {
			return
		}

		outputPath = makeTempPath()
		cmd, err := startFFmpegRecording(outputPath)
		if err != nil {
			status.SetText("Failed to start")
			lastAction.SetText(err.Error())
			return
		}

		currentCmd = cmd
		isRecording = true
		status.SetText("Recording...")
		lastAction.SetText("Saving to " + outputPath)
	})

	stopBtn := widget.NewButton("Stop Recording", func() {
		if !isRecording {
			return
		}

		isRecording = false
		err := stopFFmpegRecording(currentCmd)
		currentCmd = nil
		if err != nil && !strings.Contains(err.Error(), "signal: killed") {
			status.SetText("Stop failed")
			lastAction.SetText(err.Error())
			return
		}

		finalPath := finalizePath(outputPath)
		status.SetText("Processing subtitles...")
		lastAction.SetText("Sending to OpenRouter: " + finalPath)

		srtPath, err := makeSRTFromVideo(finalPath)
		if err != nil {
			status.SetText("Subtitle failed")
			lastAction.SetText(err.Error())
			return
		}

		status.SetText("Saved")
		lastAction.SetText("Saved video: " + finalPath + "\nSaved subtitles: " + srtPath)
	})

	content := container.NewVBox(
		widget.NewLabel("Record App"),
		status,
		startBtn,
		stopBtn,
		widget.NewLabel("Last action:"),
		lastAction,
	)

	w.SetContent(content)
	w.ShowAndRun()
}
