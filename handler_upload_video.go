package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io"
	"log"
	"mime"
	"net/http"
	"os"

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

	defer os.Remove(tempFile.Name()) // clean up
	defer tempFile.Close()

	if _, err := io.Copy(tempFile, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error copying file to tempFile", err)
		return
	}

	// this allows us to read the file again from the beginning
	tempFile.Seek(0, io.SeekStart)

	// put object into s3 bucket
	key := make([]byte, 32)
	rand.Read(key)
	randomHexString := hex.EncodeToString(key)
	fileS3Key := randomHexString + ".mp4"

	s3Params := s3.PutObjectInput{Bucket: &cfg.s3Bucket, Key: &fileS3Key, Body: tempFile, ContentType: &contentType}
	cfg.s3Client.PutObject(r.Context(), &s3Params)

	videoUrl := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, fileS3Key)

	videoMetadata.VideoURL = &videoUrl
	err = cfg.db.UpdateVideo(videoMetadata)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error updating video", err)
		return
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
