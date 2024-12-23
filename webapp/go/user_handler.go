package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/goccy/go-json"
	"github.com/google/uuid"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"
)

const (
	defaultSessionIDKey      = "SESSIONID"
	defaultSessionExpiresKey = "EXPIRES"
	defaultUserIDKey         = "USERID"
	defaultUsernameKey       = "USERNAME"
	bcryptDefaultCost        = bcrypt.MinCost
)

var fallbackImage = "../img/NoImage.jpg"
var noimage []byte

func init() {
	b, err := os.ReadFile(fallbackImage)
	if err != nil {
		panic(err)
	}
	noimage = b
}

type UserModel struct {
	ID             int64  `db:"id"`
	Name           string `db:"name"`
	DisplayName    string `db:"display_name"`
	Description    string `db:"description"`
	HashedPassword string `db:"password"`
}

type User struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	Theme       Theme  `json:"theme,omitempty"`
	IconHash    string `json:"icon_hash,omitempty"`
}

type Theme struct {
	ID       int64 `json:"id"`
	DarkMode bool  `json:"dark_mode"`
}

type ThemeModel struct {
	ID       int64 `db:"id"`
	UserID   int64 `db:"user_id"`
	DarkMode bool  `db:"dark_mode"`
}

type PostUserRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	// Password is non-hashed password.
	Password string               `json:"password"`
	Theme    PostUserRequestTheme `json:"theme"`
}

type PostUserRequestTheme struct {
	DarkMode bool `json:"dark_mode"`
}

type LoginRequest struct {
	Username string `json:"username"`
	// Password is non-hashed password.
	Password string `json:"password"`
}

type PostIconRequest struct {
	Image []byte `json:"image"`
}

type PostIconResponse struct {
	ID int64 `json:"id"`
}

func getIconHandler(c echo.Context) error {
	ctx := c.Request().Context()

	username := c.Param("username")

	var user UserModel
	if err := dbConn.GetContext(ctx, &user, "SELECT * FROM users WHERE name = ?", username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "not found user that has the given username")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user: "+err.Error())
	}

	h, err := getIconHashCache(ctx, user.ID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get icon hash: "+err.Error())
	}

	iconHashString := c.Request().Header.Get("If-None-Match")
	if iconHashString != "" {
		requestHash, err := strconv.Unquote(iconHashString)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to check If-None-Match: "+err.Error())
		}
		if h == requestHash {
			return c.NoContent(http.StatusNotModified)
		}
	}

	var image []byte
	if err := dbConn.GetContext(ctx, &image, "SELECT image FROM icons WHERE user_id = ?", user.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.File(fallbackImage)
		} else {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user icon: "+err.Error())
		}
	}

	return c.Blob(http.StatusOK, "image/jpeg", image)
}

func postIconHandler(c echo.Context) error {
	ctx := c.Request().Context()

	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	// error already checked
	sess, _ := session.Get(defaultSessionIDKey, c)
	// existence already checked
	userID := sess.Values[defaultUserIDKey].(int64)

	var req *PostIconRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "failed to decode the request body as json")
	}

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "DELETE FROM icons WHERE user_id = ?", userID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to delete old user icon: "+err.Error())
	}

	rs, err := tx.ExecContext(ctx, "INSERT INTO icons (user_id, image) VALUES (?, ?)", userID, req.Image)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to insert new user icon: "+err.Error())
	}

	iconID, err := rs.LastInsertId()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get last inserted icon id: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	iconHashCache.Delete(userID)

	return c.JSON(http.StatusCreated, &PostIconResponse{
		ID: iconID,
	})
}

func getMeHandler(c echo.Context) error {
	ctx := c.Request().Context()

	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	// error already checked
	sess, _ := session.Get(defaultSessionIDKey, c)
	// existence already checked
	userID := sess.Values[defaultUserIDKey].(int64)

	userModel := UserModel{}
	err := dbConn.GetContext(ctx, &userModel, "SELECT * FROM users WHERE id = ?", userID)
	if errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusNotFound, "not found user that has the userid in session")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user: "+err.Error())
	}

	user, err := fillUserResponseWithoutTx(ctx, userModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill user: "+err.Error())
	}

	return c.JSON(http.StatusOK, user)
}

// ユーザ登録API
// POST /api/register
func registerHandler(c echo.Context) error {
	ctx := c.Request().Context()
	defer c.Request().Body.Close()

	req := PostUserRequest{}
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "failed to decode the request body as json")
	}

	if req.Name == "pipe" {
		return echo.NewHTTPError(http.StatusBadRequest, "the username 'pipe' is reserved")
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptDefaultCost)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to generate hashed password: "+err.Error())
	}

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	userModel := UserModel{
		Name:           req.Name,
		DisplayName:    req.DisplayName,
		Description:    req.Description,
		HashedPassword: string(hashedPassword),
	}

	result, err := tx.NamedExecContext(ctx, "INSERT INTO users (name, display_name, description, password) VALUES(:name, :display_name, :description, :password)", userModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to insert user: "+err.Error())
	}

	userID, err := result.LastInsertId()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get last inserted user id: "+err.Error())
	}

	userModel.ID = userID

	themeModel := ThemeModel{
		UserID:   userID,
		DarkMode: req.Theme.DarkMode,
	}
	if _, err := tx.NamedExecContext(ctx, "INSERT INTO themes (user_id, dark_mode) VALUES(:user_id, :dark_mode)", themeModel); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to insert user theme: "+err.Error())
	}

	records.Store(req.Name+".u.isucon.local.", powerDNSSubdomainAddress)

	user, err := fillUserResponse(ctx, tx, userModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill user: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	return c.JSON(http.StatusCreated, user)
}

// ユーザログインAPI
// POST /api/login
func loginHandler(c echo.Context) error {
	ctx := c.Request().Context()
	defer c.Request().Body.Close()

	req := LoginRequest{}
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "failed to decode the request body as json")
	}

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	userModel := UserModel{}
	// usernameはUNIQUEなので、whereで一意に特定できる
	err = tx.GetContext(ctx, &userModel, "SELECT * FROM users WHERE name = ?", req.Username)
	if errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid username or password")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	err = bcrypt.CompareHashAndPassword([]byte(userModel.HashedPassword), []byte(req.Password))
	if err == bcrypt.ErrMismatchedHashAndPassword {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid username or password")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to compare hash and password: "+err.Error())
	}

	sessionEndAt := time.Now().Add(1 * time.Hour)

	sessionID := uuid.NewString()

	sess, err := session.Get(defaultSessionIDKey, c)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "failed to get session")
	}

	sess.Options = &sessions.Options{
		Domain: "u.isucon.local",
		MaxAge: int(60000),
		Path:   "/",
	}
	sess.Values[defaultSessionIDKey] = sessionID
	sess.Values[defaultUserIDKey] = userModel.ID
	sess.Values[defaultUsernameKey] = userModel.Name
	sess.Values[defaultSessionExpiresKey] = sessionEndAt.Unix()

	if err := sess.Save(c.Request(), c.Response()); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to save session: "+err.Error())
	}

	return c.NoContent(http.StatusOK)
}

// ユーザ詳細API
// GET /api/user/:username
func getUserHandler(c echo.Context) error {
	ctx := c.Request().Context()
	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	username := c.Param("username")

	userModel := UserModel{}
	if err := dbConn.GetContext(ctx, &userModel, "SELECT * FROM users WHERE name = ?", username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "not found user that has the given username")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user: "+err.Error())
	}

	user, err := fillUserResponseWithoutTx(ctx, userModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill user: "+err.Error())
	}

	return c.JSON(http.StatusOK, user)
}

func verifyUserSession(c echo.Context) error {
	sess, err := session.Get(defaultSessionIDKey, c)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "failed to get session")
	}

	sessionExpires, ok := sess.Values[defaultSessionExpiresKey]
	if !ok {
		return echo.NewHTTPError(http.StatusForbidden, "failed to get EXPIRES value from session")
	}

	_, ok = sess.Values[defaultUserIDKey].(int64)
	if !ok {
		return echo.NewHTTPError(http.StatusUnauthorized, "failed to get USERID value from session")
	}

	now := time.Now()
	if now.Unix() > sessionExpires.(int64) {
		return echo.NewHTTPError(http.StatusUnauthorized, "session has expired")
	}

	return nil
}

func fillUserResponse(ctx context.Context, tx *sqlx.Tx, userModel UserModel) (User, error) {
	themeModel := ThemeModel{}
	if err := tx.GetContext(ctx, &themeModel, "SELECT * FROM themes WHERE user_id = ?", userModel.ID); err != nil {
		return User{}, err
	}

	iconHash, err := getIconHashCache(ctx, userModel.ID)
	if err != nil {
		return User{}, err
	}

	user := User{
		ID:          userModel.ID,
		Name:        userModel.Name,
		DisplayName: userModel.DisplayName,
		Description: userModel.Description,
		Theme: Theme{
			ID:       themeModel.ID,
			DarkMode: themeModel.DarkMode,
		},
		IconHash: iconHash,
	}

	return user, nil
}

func fillUserResponseWithoutTx(ctx context.Context, userModel UserModel) (User, error) {
	themeModel := ThemeModel{}
	if err := dbConn.GetContext(ctx, &themeModel, "SELECT * FROM themes WHERE user_id = ?", userModel.ID); err != nil {
		return User{}, err
	}

	iconHash, err := getIconHashCache(ctx, userModel.ID)
	if err != nil {
		return User{}, err
	}

	user := User{
		ID:          userModel.ID,
		Name:        userModel.Name,
		DisplayName: userModel.DisplayName,
		Description: userModel.Description,
		Theme: Theme{
			ID:       themeModel.ID,
			DarkMode: themeModel.DarkMode,
		},
		IconHash: iconHash,
	}

	return user, nil
}

var iconHashCache = &IconHashCache{}

type IconHashCache struct {
	data sync.Map
}

type entry struct {
	value      string
	expiration time.Time
}

func (m *IconHashCache) Set(key int64, hash string, ttl time.Duration) {
	m.data.Store(key, entry{
		value:      hash,
		expiration: time.Now().Add(ttl),
	})
}

func (m *IconHashCache) Get(key int64) (interface{}, bool) {
	v, ok := m.data.Load(key)
	if !ok {
		return nil, false
	}

	e := v.(entry)
	if time.Now().After(e.expiration) {
		// 有効期限切れの場合は削除
		m.data.Delete(key)
		return nil, false
	}
	return e.value, true
}

func (m *IconHashCache) Delete(key int64) {
	m.data.Delete(key)
}

func (m *IconHashCache) Cleanup() {
	m.data.Range(func(key, value interface{}) bool {
		e := value.(entry)
		if time.Now().After(e.expiration) {
			m.data.Delete(key)
		}
		return true
	})
}

func (m *IconHashCache) CleanupAll() {
	m.data.Range(func(key, value interface{}) bool {
		m.data.Delete(key)
		return true
	})
}

func getIconHashCache(ctx context.Context, userID int64) (string, error) {
	v, ok := iconHashCache.Get(userID)
	if ok {
		return v.(string), nil
	}

	var image []byte
	if err := dbConn.GetContext(ctx, &image, "SELECT image FROM icons WHERE user_id = ?", userID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		image = noimage
	}

	hash := fmt.Sprintf("%x", sha256.Sum256(image))

	iconHashCache.Set(userID, hash, time.Second*2)

	return hash, nil
}

func fillUsersResponse(ctx context.Context, tx *sqlx.Tx, userModels []UserModel) ([]User, error) {
	if len(userModels) == 0 {
		return []User{}, nil
	}
	userIDs := make([]int64, len(userModels))
	for i, user := range userModels {
		userIDs[i] = user.ID
	}

	themeModels := make([]ThemeModel, len(userIDs))
	sql, params, err := sqlx.In("SELECT * FROM themes WHERE user_id IN (?)", userIDs)
	if err != nil {
		return nil, err
	}
	if err := tx.SelectContext(ctx, &themeModels, sql, params...); err != nil {
		return nil, err
	}
	themeMap := make(map[int64]ThemeModel)
	for _, theme := range themeModels {
		themeMap[theme.UserID] = theme
	}

	users := make([]User, len(userIDs))
	for i, user := range userModels {
		theme, ok := themeMap[user.ID]
		if !ok {
			return nil, fmt.Errorf("theme not found for user_id=%d", user.ID)
		}

		hash, err := getIconHashCache(ctx, user.ID)
		if err != nil {
			return nil, err
		}

		users[i] = User{
			ID:          user.ID,
			Name:        user.Name,
			DisplayName: user.DisplayName,
			Description: user.Description,
			Theme: Theme{
				ID:       theme.ID,
				DarkMode: theme.DarkMode,
			},
			IconHash: hash,
		}
	}

	return users, nil
}

func fillUsersResponseWithoutTx(ctx context.Context, userModels []UserModel) ([]User, error) {
	if len(userModels) == 0 {
		return []User{}, nil
	}
	userIDs := make([]int64, len(userModels))
	for i, user := range userModels {
		userIDs[i] = user.ID
	}

	themeModels := make([]ThemeModel, len(userIDs))
	sql, params, err := sqlx.In("SELECT * FROM themes WHERE user_id IN (?)", userIDs)
	if err != nil {
		return nil, err
	}
	if err := dbConn.SelectContext(ctx, &themeModels, sql, params...); err != nil {
		return nil, err
	}
	themeMap := make(map[int64]ThemeModel)
	for _, theme := range themeModels {
		themeMap[theme.UserID] = theme
	}

	users := make([]User, len(userIDs))
	for i, user := range userModels {
		theme, ok := themeMap[user.ID]
		if !ok {
			return nil, fmt.Errorf("theme not found for user_id=%d", user.ID)
		}

		hash, err := getIconHashCache(ctx, user.ID)
		if err != nil {
			return nil, err
		}

		users[i] = User{
			ID:          user.ID,
			Name:        user.Name,
			DisplayName: user.DisplayName,
			Description: user.Description,
			Theme: Theme{
				ID:       theme.ID,
				DarkMode: theme.DarkMode,
			},
			IconHash: hash,
		}
	}

	return users, nil
}
