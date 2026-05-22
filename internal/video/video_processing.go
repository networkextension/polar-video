package video

import (
	"context"
	"fmt"
	"os/exec"
)

func generateVideoPoster(ctx context.Context, videoPath, posterPath string) error {
	cmd := exec.CommandContext(
		ctx,
		"ffmpeg",
		"-y",
		"-ss", "00:00:00.500",
		"-i", videoPath,
		"-frames:v", "1",
		"-q:v", "2",
		posterPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg poster generation failed: %w: %s", err, string(output))
	}
	return nil
}
