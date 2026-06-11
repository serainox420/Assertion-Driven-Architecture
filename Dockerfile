FROM archlinux:latest

# Toolchain for building and running ADA inside the sandbox. This layer is cached
# and is NOT invalidated by source changes (the COPY below is the last layer), so
# day-to-day rebuilds only re-copy /ada and stay fast.
RUN pacman -Syu --noconfirm && \
    pacman -S --noconfirm go git jq base-devel curl bash && \
    pacman -Scc --noconfirm

WORKDIR /ada

# Populate /ada from the project's working directory at BUILD time. Files are
# COPIED into the image — not bind-mounted — so nothing the sandbox does can ever
# reach the host's copy of the tree. See .dockerignore for what is left out.
#   make rebuild   re-populate by rebuilding the image from scratch
#   make refresh   re-populate the running container without a rebuild
COPY . /ada
