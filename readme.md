# xoverlay

`xoverlay` is an X11 image viewer with transparency support. You need a compositor for transparency to work, e.g. [picom](https://github.com/yshui/picom).

<p align="center"><img src="https://github.com/user-attachments/assets/d3f04a0c-c7ac-43d6-ac57-8b0f1428dd69" alt="preview"></p>

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
