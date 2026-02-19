package main

import (
	"database/sql"
	"net/http"

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
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	if videoMetadata.CreateVideoParams.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Your are not authorized", err)
		return

	}
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
