#!/usr/bin/env bash
# dm-flakey — nợ #1: tạo ra torn write / lost write THẬT mà không cần rút điện.
#
# Vì sao cần: kill -9 KHÔNG bao giờ tạo torn write. Tiến trình chết nhưng page cache
# vẫn thuộc kernel và kernel vẫn writeback đầy đủ. Muốn mô phỏng mất điện phải cắt
# ở tầng thiết bị — đó chính là việc của device-mapper target "flakey".
#
# CẦN ROOT. Chỉ chạy trên máy thí nghiệm, KHÔNG chạy trên máy có dữ liệu thật.
#
#   sudo ./scripts/dm-flakey.sh up      # tạo /dev/mapper/flakey0, mkfs, mount /mnt/flakey
#   go run ./cmd/tornlab -mode write -file /mnt/flakey/torn.dat -pagesize 16384 -pages 4096 &
#   sudo ./scripts/dm-flakey.sh drop    # từ giây này mọi write bị NUỐT im lặng = mất điện
#   kill %1
#   sudo ./scripts/dm-flakey.sh heal    # thiết bị lành lại, remount
#   go run ./cmd/tornlab -mode verify -file /mnt/flakey/torn.dat -pagesize 16384
#   sudo ./scripts/dm-flakey.sh down    # dọn sạch
set -euo pipefail

IMG=${IMG:-/var/tmp/flakey.img}
SIZE_MB=${SIZE_MB:-512}
NAME=flakey0
MNT=${MNT:-/mnt/flakey}

need_root() { [ "$(id -u)" = 0 ] || { echo "cần chạy bằng sudo"; exit 1; }; }
loopdev() { losetup -j "$IMG" | cut -d: -f1; }
sectors() { blockdev --getsz "$(loopdev)"; }

case "${1:-}" in
up)
  need_root
  command -v dmsetup >/dev/null || { echo "thiếu dmsetup (apt install dmsetup)"; exit 1; }
  [ -e "$IMG" ] || truncate -s "${SIZE_MB}M" "$IMG"
  losetup -j "$IMG" | grep -q . || losetup -f "$IMG"
  LOOP=$(loopdev)
  # bảng flakey: <start> <len> flakey <dev> <offset> <up_interval> <down_interval> [features]
  # up 3600s / down 0s = thiết bị hoàn toàn lành, chưa hỏng gì.
  dmsetup create "$NAME" --table "0 $(blockdev --getsz "$LOOP") flakey $LOOP 0 3600 0"
  mkfs.ext4 -q -F "/dev/mapper/$NAME"
  mkdir -p "$MNT"
  mount "/dev/mapper/$NAME" "$MNT"
  echo "OK: /dev/mapper/$NAME đã mount tại $MNT (loop=$LOOP, img=$IMG)"
  ;;

drop)
  need_root
  LOOP=$(loopdev)
  # drop_writes: thiết bị NHẬN write và báo thành công, nhưng KHÔNG ghi gì cả.
  # Đây đúng là hành vi của một ổ mất điện giữa chừng — kể cả fsync cũng bị lừa.
  dmsetup suspend "$NAME"
  dmsetup reload "$NAME" --table "0 $(blockdev --getsz "$LOOP") flakey $LOOP 0 0 3600 1 drop_writes"
  dmsetup resume "$NAME"
  echo "ĐANG NUỐT WRITE. Mọi thứ ghi từ giờ sẽ biến mất. Giết tiến trình ghi rồi chạy: $0 heal"
  ;;

heal)
  need_root
  LOOP=$(loopdev)
  umount "$MNT" 2>/dev/null || echo "(umount lỗi — bình thường khi write đang bị nuốt)"
  dmsetup suspend "$NAME"
  dmsetup reload "$NAME" --table "0 $(blockdev --getsz "$LOOP") flakey $LOOP 0 3600 0"
  dmsetup resume "$NAME"
  # fsck sẽ cho biết CHÍNH FILESYSTEM có hỏng không — bản thân đó đã là một quan sát.
  fsck.ext4 -fn "/dev/mapper/$NAME" || echo "(fsck báo lỗi — ghi lại vào diary, đó là dữ liệu)"
  mount "/dev/mapper/$NAME" "$MNT"
  echo "OK: đã lành và mount lại. Giờ chạy tornlab -mode verify."
  ;;

down)
  need_root
  umount "$MNT" 2>/dev/null || true
  dmsetup remove "$NAME" 2>/dev/null || true
  L=$(loopdev); [ -n "$L" ] && losetup -d "$L" || true
  echo "đã dọn. (file ảnh $IMG vẫn còn, xoá tay nếu muốn)"
  ;;

status)
  dmsetup table "$NAME" 2>/dev/null || echo "chưa có thiết bị $NAME"
  ;;

*)
  sed -n '2,20p' "$0"
  ;;
esac
