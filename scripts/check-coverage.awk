NR == 1 { next }
{
    split($1, location, ":")
    file = location[1]
    total[file] += $2
    if ($3 > 0) covered[file] += $2
}
END {
    for (file in total) {
        if (total[file] > 0 && covered[file] * 100 < total[file] * 99) {
            printf "%s: %.2f%% statement coverage; requires 99%%\n", file, covered[file] * 100 / total[file]
            failed = 1
        }
    }
    exit failed
}
