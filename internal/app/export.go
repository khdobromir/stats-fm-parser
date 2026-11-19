package app

import (
	"context"
	"errors"
	"path/filepath"

	"stats-fm-parser/internal/spotify"
	"stats-fm-parser/internal/statsfm"
)

type options struct {
	statsfm.Options
	spotifyLibrary string
}

func (o options) validate() error {
	if o.spotifyLibrary != "" {
		if o.User != "" || o.Period != "" || o.From != "" || o.To != "" || o.Resume != "" || o.Mode != "" ||
			o.Update != "" || o.ImportHistory != "" || o.RefreshFrom != "" {
			return errors.New("-spotify-library нельзя сочетать с параметрами экспорта stats.fm")
		}
		return nil
	}
	return o.Options.Validate()
}

type update struct {
	statsfm.Update
	file string
	done bool
	err  error
}

func export(ctx context.Context, o options, report func(update)) (string, error) {
	if err := o.validate(); err != nil {
		return "", err
	}
	if o.spotifyLibrary != "" {
		file, count, err := spotify.Export(ctx, o.spotifyLibrary, o.Out)
		if err != nil {
			return "", err
		}
		dir := filepath.Dir(file)
		report(update{Update: statsfm.Update{Dir: dir, State: "finished", Progress: statsfm.Progress{Records: count}}, file: file})
		return dir, nil
	}
	return statsfm.Export(ctx, o.Options, func(u statsfm.Update) { report(update{Update: u}) })
}
