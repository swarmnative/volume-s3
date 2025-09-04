FROM rclone/rclone:latest

# Enable allow_other for FUSE3-based mounts
# Note: container must be run with /dev/fuse and SYS_ADMIN; host must allow_other only if non-root access is required
RUN echo user_allow_other > /etc/fuse.conf

LABEL org.opencontainers.image.source="https://github.com/swarmnative/swarm-s3-mounter" \
      org.opencontainers.image.title="volume-s3-rclone" \
      org.opencontainers.image.description="Rclone image for volume-s3 with FUSE allow_other enabled" \
      maintainer="SwarmNative"

# This image is referenced via VOLS3_RCLONE_IMAGE in volume-s3

