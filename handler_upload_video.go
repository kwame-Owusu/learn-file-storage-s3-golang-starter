package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

const uploadSizeLimit = 1 << 30 // 1gb

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	validUploadSize := checkUploadSize(w, r)
	if !validUploadSize {
		return
	}

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid video ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	videoMetadata, err := cfg.db.GetVideo(videoID)
	if err == sql.ErrNoRows {
		respondWithError(w, http.StatusNotFound, "Could not find video", err)
		return
	}

	if videoMetadata.CreateVideoParams.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Your are not authorized", err)
		return
	}

	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	// parse uploaded video file
	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	contentType := header.Header.Get("Content-Type")
	isCorrectMimeType := checkMimeTypeMp4(contentType)
	if !isCorrectMimeType {
		respondWithError(w, http.StatusBadRequest, "Incorrect mime type, need video/mp4", err)
		return
	}

	// save to temporary file on disk
	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating temp file", err)
		return
	}

	defer tempFile.Close()
	defer os.Remove(tempFile.Name()) // clean up

	if _, err := io.Copy(tempFile, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error copying file to tempFile", err)
		return
	}

	videoAspectRatio, err := getVideoAspectRatio(tempFile.Name())
	var videoS3Prefix string
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting video aspect ratio", err)
		return
	}

	switch videoAspectRatio {
	case "16:9":
		videoS3Prefix = "landscape/"
	case "9:16":
		videoS3Prefix = "portrait/"
	default:
		videoS3Prefix = "other/"
	}

	// this allows us to read the file again from the beginning
	tempFile.Seek(0, io.SeekStart)

	// put object into s3 bucket
	key := make([]byte, 32)
	rand.Read(key)
	randomHexString := hex.EncodeToString(key)
	fileS3Key := videoS3Prefix + randomHexString + ".mp4"

	processedVideoPath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video for fast start", err)
		return
	}

	processedVideoFile, err := os.Open(processedVideoPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error opening processed video file", err)
		return
	}

	defer processedVideoFile.Close()
	defer os.Remove(processedVideoPath)

	s3Params := s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileS3Key,
		Body:        processedVideoFile,
		ContentType: &contentType}
	cfg.s3Client.PutObject(r.Context(), &s3Params)

	videoUrl := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, fileS3Key)

	videoMetadata.VideoURL = &videoUrl
	err = cfg.db.UpdateVideo(videoMetadata)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error updating video", err)
		return
	}

}

func processVideoForFastStart(filepath string) (string, error) {
	outputFilePath := filepath + ".processing"

	ffmpegArgs := []string{"-i", filepath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFilePath}
	ffmpegCmd := exec.Command("ffmpeg", ffmpegArgs...)

	if err := ffmpegCmd.Run(); err != nil {
		return "", fmt.Errorf("Error running ffmpeg command, %w", err)
	}

	return outputFilePath, nil
}

func getVideoAspectRatio(filePath string) (string, error) {
	type FFProbeResult struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}

	var outBuf = new(bytes.Buffer)

	ffprobeArgs := []string{"-v", "error", "-print_format", "json", "-show_streams", filePath}
	ffpropeCmd := exec.Command("ffprobe", ffprobeArgs...)
	ffpropeCmd.Stdout = outBuf

	if err := ffpropeCmd.Run(); err != nil {
		return "", fmt.Errorf("Error running ffprope command, %w", err)
	}

	res := FFProbeResult{}
	err := json.Unmarshal(outBuf.Bytes(), &res)
	if err != nil {
		return "", fmt.Errorf("Error unmarshaling bytes into ratio struct, %w", err)
	}

	// Safety check: ensure we actually got a stream back
	if len(res.Streams) == 0 {
		return "", fmt.Errorf("no streams found in video")
	}

	aspectRatio := determineStandardRatio(res.Streams[0].Width, res.Streams[0].Height)

	return aspectRatio, nil
}

func determineStandardRatio(w, h int) string {
	if h == 0 {
		return "other"
	}

	ratio := float64(w) / float64(h)

	// Define standard ratios as decimals
	const (
		r16x9 = 16.0 / 9.0 // 1.777...
		r9x16 = 9.0 / 16.0 // 0.5625
	)

	// Use a small epsilon for "near matches"
	epsilon := 0.02

	switch {
	case math.Abs(ratio-r16x9) < epsilon:
		return "16:9"
	case math.Abs(ratio-r9x16) < epsilon:
		return "9:16"
	default:
		return "other"
	}
}

// checks if the uploaded file is an mp4 video
func checkMimeTypeMp4(contentType string) bool {
	mimeType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		log.Printf("Error parsing media type from Content-Type")
		return false
	}

	if mimeType != "video/mp4" {
		return false
	}

	return true
}

func checkUploadSize(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength > uploadSizeLimit {
		respondWithError(w, http.StatusRequestEntityTooLarge, "Content-Length too large", nil)
		return false
	}

	r.Body = http.MaxBytesReader(w, r.Body, uploadSizeLimit)

	err := r.ParseMultipartForm(uploadSizeLimit)
	if err != nil {
		// If the limit is exceeded, err will be a non-EOF error
		respondWithError(w, http.StatusRequestEntityTooLarge, "Request body too large", nil)
		return false
	}

	return true
}
