# xoverlay

`xoverlay` is an X11 image viewer with transparency support. You need a compositor for transparency to work, e.g. [picom](https://github.com/yshui/picom).

Build `xoverlay` from source:

```
git clone https://github.com/merlinzerbe/xoverlay
cd xoverlay
go build
```

Show an image:

```
./xoverlay img.png
```

Combine with a screenshot tool to quickly create an overlay window from screen content:

```
flameshot gui --raw | ./xoverlay -
```
