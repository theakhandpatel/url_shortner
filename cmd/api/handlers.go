package main

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"url_shortner/internal/data"
	"url_shortner/internal/validator"

	"github.com/go-chi/chi"
)

// health check message.
func (app *application) HealthCheckHandler(w http.ResponseWriter, r *http.Request) {
	app.writeJSON(w, http.StatusOK, envelope{"message": "OK"})
}

// URL shortening requests.
func (app *application) ShortenURLHandler(w http.ResponseWriter, r *http.Request) {

	var input inputURL
	err := app.readJSON(w, r, &input)

	if err != nil {
		app.badRequestResponse(w, r, err)
		return
	}

	v := validator.New()
	ValidateInput(v, &input)
	if !v.Valid() {
		app.failedValidationResponse(w, r, v.Errors)
		return
	}

	var url *data.URL

	//If no custom code is required
	if input.ShortURL == "" {
		// if the URL already exists in the database.
		existingURL, err := app.Model.URLS.GetByLongURL(input.LongURL)
		if err != nil && err != data.ErrRecordNotFound {
			app.serverErrorResponse(w, r, err)
			return
		}

		if existingURL != nil && existingURL.Long == input.LongURL {
			app.writeJSON(w, http.StatusOK, envelope{"url": existingURL})
			return
		}
	}

	url = data.NewURL(input.LongURL, input.ShortURL)

	urlInserted := false

	for retriesLeft := app.config.MaxCollisionRetries; retriesLeft > 0; retriesLeft-- {
		err := app.Model.URLS.Insert(url)
		if err == nil {
			urlInserted = true
			break
		}

		if err != data.ErrDuplicateEntry {
			app.serverErrorResponse(w, r, err)
			return
		}

		if err == data.ErrDuplicateEntry {
			url.ReShorten() //  modify the short code
		}
	}

	if !urlInserted {
		app.serverErrorResponse(w, r, err)
		return
	}

	app.writeJSON(w, http.StatusCreated, envelope{"url": url})
}

// expanding short URLs.
func (app *application) ExpandURLHandler(w http.ResponseWriter, r *http.Request) {
	shortURL := chi.URLParam(r, "shortURL")

	url, err := app.Model.URLS.Get(shortURL)
	if err != nil {
		switch {

		case errors.Is(err, data.ErrRecordNotFound):
			app.NotFoundResponse(w, r)
		default:
			app.serverErrorResponse(w, r, err)
		}
		return
	}

	// Redirect to the long URL.
	longURL := url.Long
	if longURL == "" {
		app.NotFoundResponse(w, r)
		return
	}

	// Update access count and record analytics.
	err = app.Model.URLS.UpdateCount(shortURL)
	if err != nil {
		app.logResponse(r, err)
	}

	analyticsEntry := data.AnalyticsEntry{
		ShortURL:  shortURL,
		IP:        r.RemoteAddr,
		UserAgent: r.UserAgent(),
		Referrer:  r.Referer(),
		Timestamp: time.Now(),
	}

	err = app.Model.Analytics.Insert(&analyticsEntry)
	if err != nil {
		app.logResponse(r, err)
	}

	http.Redirect(w, r, longURL, app.config.StatusRedirectType)
}

// analytics for a given short URL.
func (app *application) AnalyticsHandler(w http.ResponseWriter, r *http.Request) {
	shortURL := r.URL.Query().Get("URL")
	if shortURL == "" {
		app.badRequestResponse(w, r, errors.New("url parameter is missing"))
		return
	}

	shortCode, err := extractShortcode(shortURL)
	if err != nil {
		app.NotFoundResponse(w, r)
		return
	}

	analytics, err := app.Model.Analytics.Get(shortCode)
	if err != nil {
		app.serverErrorResponse(w, r, err)
	}

	app.writeJSON(w, http.StatusOK, envelope{"short_url": shortURL, "analytics": analytics})
}

func (app *application) registerUserHandler(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	err := app.readJSON(w, r, &input)
	if err != nil {
		app.badRequestResponse(w, r, err)
		return
	}

	user := &data.User{
		Name:      input.Name,
		Email:     strings.ToLower(input.Email),
		Activated: false,
	}

	err = user.Password.Set(input.Password)
	if err != nil {
		app.serverErrorResponse(w, r, err)
		return
	}

	v := validator.New()
	data.ValidateUser(v, user)

	if !v.Valid() {
		app.failedValidationResponse(w, r, v.Errors)
		return
	}

	err = app.Model.Users.Insert(user)
	if err != nil {
		switch {
		case errors.Is(err, data.ErrDuplicateEmail):
			v.AddError("email", "A user with this email address already exists")
			app.failedValidationResponse(w, r, v.Errors)
		default:
			app.serverErrorResponse(w, r, err)
		}
		return
	}

	err = app.writeJSON(w, http.StatusAccepted, envelope{"user": user})
	if err != nil {
		app.serverErrorResponse(w, r, err)
	}
}

func (app *application) loginUserHandler(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	err := app.readJSON(w, r, &input)
	if err != nil {
		app.badRequestResponse(w, r, err)
		return
	}

	input.Email = strings.ToLower(input.Email)
	v := validator.New()
	data.ValidateEmail(v, input.Email)
	data.ValidatePasswordPlaintext(v, input.Password)

	if !v.Valid() {
		app.failedValidationResponse(w, r, v.Errors)
		return
	}

	user, err := app.Model.Users.GetByEmail(input.Email)
	if err != nil {
		switch {
		case errors.Is(err, data.ErrRecordNotFound):
			app.invalidCredentialsResponse(w, r)
		default:
			app.serverErrorResponse(w, r, err)
		}
		return
	}

	match, err := user.Password.Matches(input.Password)
	if err != nil {
		app.serverErrorResponse(w, r, err)
		return
	}

	if !match {
		app.invalidCredentialsResponse(w, r)
		return
	}

	token, err := app.Model.Tokens.New(user.ID, 24*time.Hour, data.ScopeAuthentication)
	if err != nil {
		app.serverErrorResponse(w, r, err)
		return
	}

	err = app.writeJSON(w, http.StatusCreated, envelope{"authentication_token": token})
	if err != nil {
		app.serverErrorResponse(w, r, err)
		return
	}
}

// TODO: Implement Logout
func (app *application) logoutUserHandler(w http.ResponseWriter, r *http.Request) {

	var input struct {
		Token string `json:"token"`
	}

	app.readJSON(w, r, &input)

	err := app.Model.Tokens.DeleteOneForUser(input.Token)
	if err != nil {
		app.serverErrorResponse(w, r, err)
		return
	}
	app.writeJSON(w, http.StatusOK, envelope{"message": "logged out"})
}
