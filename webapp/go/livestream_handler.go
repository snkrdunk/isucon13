package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/goccy/go-json"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
)

type ReserveLivestreamRequest struct {
	Tags         []int64 `json:"tags"`
	Title        string  `json:"title"`
	Description  string  `json:"description"`
	PlaylistUrl  string  `json:"playlist_url"`
	ThumbnailUrl string  `json:"thumbnail_url"`
	StartAt      int64   `json:"start_at"`
	EndAt        int64   `json:"end_at"`
}

type LivestreamViewerModel struct {
	UserID       int64 `db:"user_id" json:"user_id"`
	LivestreamID int64 `db:"livestream_id" json:"livestream_id"`
	CreatedAt    int64 `db:"created_at" json:"created_at"`
}

type LivestreamModel struct {
	ID           int64  `db:"id" json:"id"`
	UserID       int64  `db:"user_id" json:"user_id"`
	Title        string `db:"title" json:"title"`
	Description  string `db:"description" json:"description"`
	PlaylistUrl  string `db:"playlist_url" json:"playlist_url"`
	ThumbnailUrl string `db:"thumbnail_url" json:"thumbnail_url"`
	StartAt      int64  `db:"start_at" json:"start_at"`
	EndAt        int64  `db:"end_at" json:"end_at"`
}

type Livestream struct {
	ID           int64  `json:"id"`
	Owner        User   `json:"owner"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	PlaylistUrl  string `json:"playlist_url"`
	ThumbnailUrl string `json:"thumbnail_url"`
	Tags         []Tag  `json:"tags"`
	StartAt      int64  `json:"start_at"`
	EndAt        int64  `json:"end_at"`
}

type LivestreamTagModel struct {
	ID           int64 `db:"id" json:"id"`
	LivestreamID int64 `db:"livestream_id" json:"livestream_id"`
	TagID        int64 `db:"tag_id" json:"tag_id"`
}

type ReservationSlotModels []ReservationSlotModel

func (ms ReservationSlotModels) GetSlotCount(slot ReservationSlotModel) int64 {
	for _, m := range ms {
		if m.StartAt == slot.StartAt && m.EndAt == slot.EndAt {
			return m.Slot
		}
	}
	return 0
}

type ReservationSlotModel struct {
	ID      int64 `db:"id" json:"id"`
	Slot    int64 `db:"slot" json:"slot"`
	StartAt int64 `db:"start_at" json:"start_at"`
	EndAt   int64 `db:"end_at" json:"end_at"`
}

func reserveLivestreamHandler(c echo.Context) error {
	ctx := c.Request().Context()
	defer c.Request().Body.Close()

	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	// error already checked
	sess, _ := session.Get(defaultSessionIDKey, c)
	// existence already checked
	userID := sess.Values[defaultUserIDKey].(int64)

	var req *ReserveLivestreamRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "failed to decode the request body as json")
	}

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	// 2023/11/25 10:00からの１年間の期間内であるかチェック
	var (
		termStartAt    = time.Date(2023, 11, 25, 1, 0, 0, 0, time.UTC)
		termEndAt      = time.Date(2024, 11, 25, 1, 0, 0, 0, time.UTC)
		reserveStartAt = time.Unix(req.StartAt, 0)
		reserveEndAt   = time.Unix(req.EndAt, 0)
	)
	if (reserveStartAt.Equal(termEndAt) || reserveStartAt.After(termEndAt)) || (reserveEndAt.Equal(termStartAt) || reserveEndAt.Before(termStartAt)) {
		return echo.NewHTTPError(http.StatusBadRequest, "bad reservation time range")
	}

	// 予約枠をみて、予約が可能か調べる
	// NOTE: 並列な予約のoverbooking防止にFOR UPDATEが必要
	var slots ReservationSlotModels
	if err := tx.SelectContext(ctx, &slots, "SELECT * FROM reservation_slots WHERE start_at >= ? AND end_at <= ? FOR UPDATE", req.StartAt, req.EndAt); err != nil {
		c.Logger().Warnf("予約枠一覧取得でエラー発生: %+v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get reservation_slots: "+err.Error())
	}
	for _, slot := range slots {
		count := slots.GetSlotCount(slot)
		c.Logger().Infof("%d ~ %d予約枠の残数 = %d\n", slot.StartAt, slot.EndAt, slot.Slot)
		if count < 1 {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("予約期間 %d ~ %dに対して、予約区間 %d ~ %dが予約できません", termStartAt.Unix(), termEndAt.Unix(), req.StartAt, req.EndAt))
		}
	}

	var (
		livestreamModel = &LivestreamModel{
			UserID:       int64(userID),
			Title:        req.Title,
			Description:  req.Description,
			PlaylistUrl:  req.PlaylistUrl,
			ThumbnailUrl: req.ThumbnailUrl,
			StartAt:      req.StartAt,
			EndAt:        req.EndAt,
		}
	)

	if _, err := tx.ExecContext(ctx, "UPDATE reservation_slots SET slot = slot - 1 WHERE start_at >= ? AND end_at <= ?", req.StartAt, req.EndAt); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to update reservation_slot: "+err.Error())
	}

	rs, err := tx.NamedExecContext(ctx, "INSERT INTO livestreams (user_id, title, description, playlist_url, thumbnail_url, start_at, end_at) VALUES(:user_id, :title, :description, :playlist_url, :thumbnail_url, :start_at, :end_at)", livestreamModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to insert livestream: "+err.Error())
	}

	livestreamID, err := rs.LastInsertId()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get last inserted livestream id: "+err.Error())
	}
	livestreamModel.ID = livestreamID

	// タグ追加
	for _, tagID := range req.Tags {
		if _, err := tx.NamedExecContext(ctx, "INSERT INTO livestream_tags (livestream_id, tag_id) VALUES (:livestream_id, :tag_id)", &LivestreamTagModel{
			LivestreamID: livestreamID,
			TagID:        tagID,
		}); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to insert livestream tag: "+err.Error())
		}
	}

	livestream, err := fillLivestreamResponse(ctx, tx, *livestreamModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill livestream: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	return c.JSON(http.StatusCreated, livestream)
}

func searchLivestreamsHandler(c echo.Context) error {
	ctx := c.Request().Context()
	keyTagName := c.QueryParam("tag")

	var livestreamModels []LivestreamModel
	if c.QueryParam("tag") != "" {
		// タグによる取得
		var tagIDList []int
		if err := dbConn.SelectContext(ctx, &tagIDList, "SELECT id FROM tags WHERE name = ?", keyTagName); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get tags: "+err.Error())
		}

		if len(tagIDList) > 0 {
			var keyTaggedLivestreams []LivestreamTagModel
			query, params, err := sqlx.In("SELECT * FROM livestream_tags WHERE tag_id IN (?) ORDER BY livestream_id DESC", tagIDList)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, "failed to construct IN query: "+err.Error())
			}
			if err := dbConn.SelectContext(ctx, &keyTaggedLivestreams, query, params...); err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, "failed to get keyTaggedLivestreams: "+err.Error())
			}

			if len(keyTaggedLivestreams) > 0 {
				livestreamIDs := make([]int64, len(keyTaggedLivestreams))
				for i := range keyTaggedLivestreams {
					livestreamIDs[i] = keyTaggedLivestreams[i].LivestreamID
				}
				query, params, err := sqlx.In("SELECT * FROM livestreams WHERE id IN (?) ORDER BY id DESC", livestreamIDs)
				if err != nil {
					return echo.NewHTTPError(http.StatusInternalServerError, "failed to construct IN query: "+err.Error())
				}
				if err := dbConn.SelectContext(ctx, &livestreamModels, query, params...); err != nil {
					return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livestreams: "+err.Error())
				}
			}
		}
	} else {
		// 検索条件なし
		query := `SELECT * FROM livestreams ORDER BY id DESC`
		if c.QueryParam("limit") != "" {
			limit, err := strconv.Atoi(c.QueryParam("limit"))
			if err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, "limit query parameter must be integer")
			}
			query += fmt.Sprintf(" LIMIT %d", limit)
		}

		if err := dbConn.SelectContext(ctx, &livestreamModels, query); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livestreams: "+err.Error())
		}
	}

	livestreams, err := fillLivestreamsResponseWithoutTx(ctx, livestreamModels)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill livestreams: "+err.Error())
	}

	return c.JSON(http.StatusOK, livestreams)
}

func getMyLivestreamsHandler(c echo.Context) error {
	ctx := c.Request().Context()
	if err := verifyUserSession(c); err != nil {
		return err
	}

	// error already checked
	sess, _ := session.Get(defaultSessionIDKey, c)
	// existence already checked
	userID := sess.Values[defaultUserIDKey].(int64)

	var livestreamModels []*LivestreamModel
	if err := dbConn.SelectContext(ctx, &livestreamModels, "SELECT * FROM livestreams WHERE user_id = ?", userID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livestreams: "+err.Error())
	}
	livestreams := make([]Livestream, len(livestreamModels))
	for i := range livestreamModels {
		livestream, err := fillLivestreamResponseWithoutTx(ctx, *livestreamModels[i])
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill livestream: "+err.Error())
		}
		livestreams[i] = livestream
	}

	return c.JSON(http.StatusOK, livestreams)
}

func getUserLivestreamsHandler(c echo.Context) error {
	ctx := c.Request().Context()
	if err := verifyUserSession(c); err != nil {
		return err
	}

	username := c.Param("username")

	var user UserModel
	if err := dbConn.GetContext(ctx, &user, "SELECT * FROM users WHERE name = ?", username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "user not found")
		} else {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user: "+err.Error())
		}
	}

	var livestreamModels []*LivestreamModel
	if err := dbConn.SelectContext(ctx, &livestreamModels, "SELECT * FROM livestreams WHERE user_id = ?", user.ID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livestreams: "+err.Error())
	}
	livestreams := make([]Livestream, len(livestreamModels))
	for i := range livestreamModels {
		livestream, err := fillLivestreamResponseWithoutTx(ctx, *livestreamModels[i])
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill livestream: "+err.Error())
		}
		livestreams[i] = livestream
	}

	return c.JSON(http.StatusOK, livestreams)
}

// viewerテーブルの廃止
func enterLivestreamHandler(c echo.Context) error {
	ctx := c.Request().Context()
	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	// error already checked
	sess, _ := session.Get(defaultSessionIDKey, c)
	// existence already checked
	userID := sess.Values[defaultUserIDKey].(int64)

	livestreamID, err := strconv.Atoi(c.Param("livestream_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livestream_id must be integer")
	}

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	viewer := LivestreamViewerModel{
		UserID:       int64(userID),
		LivestreamID: int64(livestreamID),
		CreatedAt:    time.Now().Unix(),
	}

	if _, err := tx.NamedExecContext(ctx, "INSERT INTO livestream_viewers_history (user_id, livestream_id, created_at) VALUES(:user_id, :livestream_id, :created_at)", viewer); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to insert livestream_view_history: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	return c.NoContent(http.StatusOK)
}

func exitLivestreamHandler(c echo.Context) error {
	ctx := c.Request().Context()
	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	// error already checked
	sess, _ := session.Get(defaultSessionIDKey, c)
	// existence already checked
	userID := sess.Values[defaultUserIDKey].(int64)

	livestreamID, err := strconv.Atoi(c.Param("livestream_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livestream_id in path must be integer")
	}

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "DELETE FROM livestream_viewers_history WHERE user_id = ? AND livestream_id = ?", userID, livestreamID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to delete livestream_view_history: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	return c.NoContent(http.StatusOK)
}

func getLivestreamHandler(c echo.Context) error {
	ctx := c.Request().Context()

	if err := verifyUserSession(c); err != nil {
		return err
	}

	livestreamID, err := strconv.Atoi(c.Param("livestream_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livestream_id in path must be integer")
	}

	livestreamModel := LivestreamModel{}
	err = dbConn.GetContext(ctx, &livestreamModel, "SELECT * FROM livestreams WHERE id = ?", livestreamID)
	if errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusNotFound, "not found livestream that has the given id")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livestream: "+err.Error())
	}

	livestream, err := fillLivestreamResponseWithoutTx(ctx, livestreamModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill livestream: "+err.Error())
	}

	return c.JSON(http.StatusOK, livestream)
}

func getLivecommentReportsHandler(c echo.Context) error {
	ctx := c.Request().Context()

	if err := verifyUserSession(c); err != nil {
		return err
	}

	livestreamID, err := strconv.Atoi(c.Param("livestream_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livestream_id in path must be integer")
	}

	var livestreamModel LivestreamModel
	if err := dbConn.GetContext(ctx, &livestreamModel, "SELECT * FROM livestreams WHERE id = ?", livestreamID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livestream: "+err.Error())
	}

	// error already check
	sess, _ := session.Get(defaultSessionIDKey, c)
	// existence already check
	userID := sess.Values[defaultUserIDKey].(int64)

	if livestreamModel.UserID != userID {
		return echo.NewHTTPError(http.StatusForbidden, "can't get other streamer's livecomment reports")
	}

	var reportModels []*LivecommentReportModel
	if err := dbConn.SelectContext(ctx, &reportModels, "SELECT * FROM livecomment_reports WHERE livestream_id = ?", livestreamID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livecomment reports: "+err.Error())
	}

	reports := make([]LivecommentReport, len(reportModels))
	for i := range reportModels {
		report, err := fillLivecommentReportResponseWithoutTx(ctx, *reportModels[i])
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill livecomment report: "+err.Error())
		}
		reports[i] = report
	}

	return c.JSON(http.StatusOK, reports)
}

func fillLivestreamResponse(ctx context.Context, tx *sqlx.Tx, livestreamModel LivestreamModel) (Livestream, error) {
	ownerModel := UserModel{}
	if err := tx.GetContext(ctx, &ownerModel, "SELECT * FROM users WHERE id = ?", livestreamModel.UserID); err != nil {
		return Livestream{}, err
	}
	owner, err := fillUserResponse(ctx, tx, ownerModel)
	if err != nil {
		return Livestream{}, err
	}

	var livestreamTagModels []*LivestreamTagModel
	if err := tx.SelectContext(ctx, &livestreamTagModels, "SELECT * FROM livestream_tags WHERE livestream_id = ?", livestreamModel.ID); err != nil {
		return Livestream{}, err
	}

	livestream := Livestream{
		ID:           livestreamModel.ID,
		Owner:        owner,
		Title:        livestreamModel.Title,
		Tags:         []Tag{},
		Description:  livestreamModel.Description,
		PlaylistUrl:  livestreamModel.PlaylistUrl,
		ThumbnailUrl: livestreamModel.ThumbnailUrl,
		StartAt:      livestreamModel.StartAt,
		EndAt:        livestreamModel.EndAt,
	}

	if len(livestreamTagModels) > 0 {
		tagIDs := make([]int64, len(livestreamTagModels))
		for i := range livestreamTagModels {
			tagIDs[i] = livestreamTagModels[i].TagID
		}
		query, params, err := sqlx.In("SELECT * FROM tags WHERE id IN (?)", tagIDs)
		if err != nil {
			return Livestream{}, err
		}
		tagModels := []TagModel{}
		if err := tx.SelectContext(ctx, &tagModels, query, params...); err != nil {
			return Livestream{}, err
		}
		livestream.Tags = make([]Tag, len(tagModels))
		for i := range tagModels {
			livestream.Tags[i] = Tag{
				ID:   tagModels[i].ID,
				Name: tagModels[i].Name,
			}
		}
	}

	return livestream, nil
}

func fillLivestreamResponseWithoutTx(ctx context.Context, livestreamModel LivestreamModel) (Livestream, error) {
	ownerModel := UserModel{}
	if err := dbConn.GetContext(ctx, &ownerModel, "SELECT * FROM users WHERE id = ?", livestreamModel.UserID); err != nil {
		return Livestream{}, err
	}
	owner, err := fillUserResponseWithoutTx(ctx, ownerModel)
	if err != nil {
		return Livestream{}, err
	}

	var livestreamTagModels []*LivestreamTagModel
	if err := dbConn.SelectContext(ctx, &livestreamTagModels, "SELECT * FROM livestream_tags WHERE livestream_id = ?", livestreamModel.ID); err != nil {
		return Livestream{}, err
	}

	livestream := Livestream{
		ID:           livestreamModel.ID,
		Owner:        owner,
		Title:        livestreamModel.Title,
		Tags:         []Tag{},
		Description:  livestreamModel.Description,
		PlaylistUrl:  livestreamModel.PlaylistUrl,
		ThumbnailUrl: livestreamModel.ThumbnailUrl,
		StartAt:      livestreamModel.StartAt,
		EndAt:        livestreamModel.EndAt,
	}

	if len(livestreamTagModels) > 0 {
		tagIDs := make([]int64, len(livestreamTagModels))
		for i := range livestreamTagModels {
			tagIDs[i] = livestreamTagModels[i].TagID
		}
		query, params, err := sqlx.In("SELECT * FROM tags WHERE id IN (?)", tagIDs)
		if err != nil {
			return Livestream{}, err
		}
		tagModels := []TagModel{}
		if err := dbConn.SelectContext(ctx, &tagModels, query, params...); err != nil {
			return Livestream{}, err
		}
		livestream.Tags = make([]Tag, len(tagModels))
		for i := range tagModels {
			livestream.Tags[i] = Tag{
				ID:   tagModels[i].ID,
				Name: tagModels[i].Name,
			}
		}
	}

	return livestream, nil
}

func fillLivestreamsResponse(ctx context.Context, tx *sqlx.Tx, livestreamModels []LivestreamModel) ([]Livestream, error) {
	if len(livestreamModels) == 0 {
		return []Livestream{}, nil
	}
	ownerUserIDs := make([]int64, len(livestreamModels))
	for i := range livestreamModels {
		ownerUserIDs[i] = livestreamModels[i].UserID
	}
	sql, params, err := sqlx.In(`SELECT * FROM users WHERE id IN (?)`, ownerUserIDs)
	if err != nil {
		return nil, err
	}
	ownerModels := []UserModel{}
	if err := tx.SelectContext(ctx, &ownerModels, sql, params...); err != nil {
		return nil, err
	}
	owners, err := fillUsersResponse(ctx, tx, ownerModels)
	if err != nil {
		return nil, err
	}
	ownersMap := make(map[int64]User)
	for i := range owners {
		ownersMap[owners[i].ID] = owners[i]
	}

	livestreamIDs := make([]int64, len(livestreamModels))
	for i := range livestreamModels {
		livestreamIDs[i] = livestreamModels[i].ID
	}
	sql, params, err = sqlx.In(`SELECT lt.livestream_id AS livestream_id, t.id AS tag_id, t.name AS tag_name FROM livestream_tags AS lt JOIN tags AS t ON lt.tag_id=t.id WHERE lt.livestream_id IN (?)`, livestreamIDs)
	if err != nil {
		return nil, err
	}
	type LivestreamTag struct {
		LivestreamID int64  `db:"livestream_id"`
		TagID        int64  `db:"tag_id"`
		TagName      string `db:"tag_name"`
	}
	livestreamTagModels := []LivestreamTag{}
	if err := tx.SelectContext(ctx, &livestreamTagModels, sql, params...); err != nil {
		return nil, err
	}
	livestreamTagMap := make(map[int64][]Tag)
	for i := range livestreamTagModels {
		livestreamTagMap[livestreamTagModels[i].LivestreamID] = append(livestreamTagMap[livestreamTagModels[i].LivestreamID], Tag{
			ID:   livestreamTagModels[i].TagID,
			Name: livestreamTagModels[i].TagName,
		})
	}

	livestreams := make([]Livestream, len(livestreamModels))
	for i := range livestreamModels {
		owner, ok := ownersMap[livestreamModels[i].UserID]
		if !ok {
			return nil, fmt.Errorf("owner not found for livestream id %d", livestreamModels[i].ID)
		}
		livestreams[i] = Livestream{
			ID:           livestreamModels[i].ID,
			Owner:        owner,
			Title:        livestreamModels[i].Title,
			Tags:         livestreamTagMap[livestreamModels[i].ID],
			Description:  livestreamModels[i].Description,
			PlaylistUrl:  livestreamModels[i].PlaylistUrl,
			ThumbnailUrl: livestreamModels[i].ThumbnailUrl,
			StartAt:      livestreamModels[i].StartAt,
			EndAt:        livestreamModels[i].EndAt,
		}
		if len(livestreams[i].Tags) == 0 {
			livestreams[i].Tags = []Tag{}
		}
	}
	return livestreams, nil
}

func fillLivestreamsResponseWithoutTx(ctx context.Context, livestreamModels []LivestreamModel) ([]Livestream, error) {
	if len(livestreamModels) == 0 {
		return []Livestream{}, nil
	}
	ownerUserIDs := make([]int64, len(livestreamModels))
	for i := range livestreamModels {
		ownerUserIDs[i] = livestreamModels[i].UserID
	}
	sql, params, err := sqlx.In(`SELECT * FROM users WHERE id IN (?)`, ownerUserIDs)
	if err != nil {
		return nil, err
	}
	ownerModels := []UserModel{}
	if err := dbConn.SelectContext(ctx, &ownerModels, sql, params...); err != nil {
		return nil, err
	}
	owners, err := fillUsersResponseWithoutTx(ctx, ownerModels)
	if err != nil {
		return nil, err
	}
	ownersMap := make(map[int64]User)
	for i := range owners {
		ownersMap[owners[i].ID] = owners[i]
	}

	livestreamIDs := make([]int64, len(livestreamModels))
	for i := range livestreamModels {
		livestreamIDs[i] = livestreamModels[i].ID
	}
	sql, params, err = sqlx.In(`SELECT lt.livestream_id AS livestream_id, t.id AS tag_id, t.name AS tag_name FROM livestream_tags AS lt JOIN tags AS t ON lt.tag_id=t.id WHERE lt.livestream_id IN (?)`, livestreamIDs)
	if err != nil {
		return nil, err
	}
	type LivestreamTag struct {
		LivestreamID int64  `db:"livestream_id"`
		TagID        int64  `db:"tag_id"`
		TagName      string `db:"tag_name"`
	}
	livestreamTagModels := []LivestreamTag{}
	if err := dbConn.SelectContext(ctx, &livestreamTagModels, sql, params...); err != nil {
		return nil, err
	}
	livestreamTagMap := make(map[int64][]Tag)
	for i := range livestreamTagModels {
		livestreamTagMap[livestreamTagModels[i].LivestreamID] = append(livestreamTagMap[livestreamTagModels[i].LivestreamID], Tag{
			ID:   livestreamTagModels[i].TagID,
			Name: livestreamTagModels[i].TagName,
		})
	}

	livestreams := make([]Livestream, len(livestreamModels))
	for i := range livestreamModels {
		owner, ok := ownersMap[livestreamModels[i].UserID]
		if !ok {
			return nil, fmt.Errorf("owner not found for livestream id %d", livestreamModels[i].ID)
		}
		livestreams[i] = Livestream{
			ID:           livestreamModels[i].ID,
			Owner:        owner,
			Title:        livestreamModels[i].Title,
			Tags:         livestreamTagMap[livestreamModels[i].ID],
			Description:  livestreamModels[i].Description,
			PlaylistUrl:  livestreamModels[i].PlaylistUrl,
			ThumbnailUrl: livestreamModels[i].ThumbnailUrl,
			StartAt:      livestreamModels[i].StartAt,
			EndAt:        livestreamModels[i].EndAt,
		}
		if len(livestreams[i].Tags) == 0 {
			livestreams[i].Tags = []Tag{}
		}
	}
	return livestreams, nil
}
