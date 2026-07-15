{ pkgs }:
pkgs.buildFHSEnv {
  name = "herdr-playwright-fhs";
  targetPkgs = pkgs: with pkgs; [
    glib nss nspr dbus atk at-spi2-atk cups expat libdrm libxkbcommon
    mesa libgbm libglvnd vulkan-loader pango cairo alsa-lib systemd gtk4
    gdk-pixbuf harfbuzz harfbuzzFull graphene icu74 libxml2 libxml2_13
    sqlite libxslt lcms2 libevent libopus libgcrypt libgpg-error flite
    libwebp libavif libepoxy libjpeg libpng freetype fontconfig wayland
    libmanette enchant libsecret libpsl nghttp2 woff2 hyphen libtasn1 zlib
    libjpeg8 x264 libx11 libxcb libxcomposite libxdamage libxext libxfixes
    libxrandr libxrender libxtst xorgserver
    gst_all_1.gstreamer gst_all_1.gst-plugins-base gst_all_1.gst-plugins-good
    gst_all_1.gst-plugins-bad
  ];
  extraBwrapArgs = [ "--bind /tmp /tmp" ];
  runScript = "bash";
}
