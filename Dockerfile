FROM archlinux:latest

RUN pacman -Syu --noconfirm && \
    pacman -S --noconfirm go git jq base-devel curl bash && \
    pacman -Scc --noconfirm

WORKDIR /ada
