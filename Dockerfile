# Lambda container image (provided.al2023 + binary Go).
# Binary được build trước ở bước CI (hoặc lệnh trong README) để Docker không cần quyền đọc module private.
FROM public.ecr.aws/lambda/provided:al2023
COPY dist/bootstrap ./bootstrap
ENTRYPOINT ["./bootstrap"]
