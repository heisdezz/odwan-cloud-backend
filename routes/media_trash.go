package routes

import (
	"context"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"net/http"
	"time"
)

const trashRetention = 30 * 24 * time.Hour

func registerTrashRoutes(se *core.ServeEvent) {
	se.Router.GET("/api/media/capabilities", func(e *core.RequestEvent) error {
		if e.Auth == nil {
			return e.UnauthorizedError("Log in to continue", nil)
		}
		return e.JSON(http.StatusOK, map[string]any{"trash": true, "retention_days": 30, "cloud_deletion": true})
	})
	se.Router.POST("/api/media/{id}/trash", mediaTrashHandler("trash"))
	se.Router.POST("/api/media/{id}/restore", mediaTrashHandler("restore"))
	se.Router.DELETE("/api/media/{id}/permanent", mediaTrashHandler("permanent"))
}

func mediaTrashHandler(action string) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		if e.Auth == nil {
			return e.UnauthorizedError("Log in to continue", nil)
		}
		info, err := e.RequestInfo()
		if err != nil {
			return err
		}
		err = e.App.RunInTransaction(func(tx core.App) error {
			record, err := tx.FindRecordById("media_item", e.Request.PathValue("id"))
			if err != nil {
				return e.NotFoundError("Media not found", err)
			}
			rule := record.Collection().UpdateRule
			if action == "permanent" {
				rule = record.Collection().DeleteRule
			}
			allowed, err := tx.CanAccessRecord(record, info, rule)
			if err != nil || !allowed {
				return e.ForbiddenError("You cannot change this media", err)
			}
			now := time.Now().UnixMilli()
			switch action {
			case "trash":
				if record.GetInt("trashed_at") > 0 {
					return nil
				}
				record.Set("trashed_at", now)
				record.Set("trash_expires_at", now+trashRetention.Milliseconds())
			case "restore":
				if record.GetInt("trashed_at") == 0 {
					return nil
				}
				if int64(record.GetInt("trash_expires_at")) <= now {
					return e.BadRequestError("Recovery period has expired", nil)
				}
				record.Set("trashed_at", 0)
				record.Set("trash_expires_at", 0)
			case "permanent":
				if record.GetInt("trashed_at") == 0 {
					return e.BadRequestError("Move this item to trash first", nil)
				}
				return tx.Delete(record)
			default:
				return e.BadRequestError("Unknown trash action", nil)
			}
			return tx.Save(record)
		})
		if err != nil {
			return err
		}
		return e.JSON(http.StatusOK, map[string]bool{"ok": true})
	}
}

func purgeExpiredMedia(ctx context.Context, app core.App) {
	var ids []string
	now := time.Now().UnixMilli()
	err := app.DB().NewQuery(`SELECT id FROM media_item WHERE trashed_at > 0 AND trash_expires_at <= {:now} ORDER BY trash_expires_at LIMIT 10`).Bind(dbx.Params{"now": now}).Column(&ids)
	if err != nil {
		app.Logger().Error("read expired trash", "error", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		err := app.RunInTransaction(func(tx core.App) error {
			record, err := tx.FindRecordById("media_item", id)
			if err != nil {
				return err
			}
			// Recheck inside the transaction: a concurrent restore must win safely.
			if record.GetInt("trashed_at") == 0 || int64(record.GetInt("trash_expires_at")) > now {
				return nil
			}
			return tx.Delete(record)
		})
		if err != nil {
			app.Logger().Error("purge expired media", "id", id, "error", err)
		}
	}
}
