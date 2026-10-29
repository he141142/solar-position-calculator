# Lambda container image (provided.al2023 + binary Go).
# Binary được build trước ở bước CI (hoặc lệnh trong README) để Docker không cần quyền đọc module private.
FROM public.ecr.aws/lambda/provided:al2023
# --chmod: artifact của GitHub Actions (và build trên Windows) làm mất quyền thực thi của binary
# -> không có dòng này Lambda báo "permission denied" khi được gọi.
COPY --chmod=0755 dist/bootstrap ./bootstrap
ENTRYPOINT ["./bootstrap"]
