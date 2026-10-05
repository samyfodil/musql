/* Memory: globals that point at globals, structs with padding, arrays of
 * structs, byte copies and fills, and pointer arithmetic both ways. */
#define memcpy __builtin_memcpy
#define memmove __builtin_memmove
#define memset __builtin_memset
#define strlen __builtin_strlen

struct item { char tag; int weight; short n; long id; };

static const char name[] = "musql";
static const char *names[] = { name, name + 2, "vdbe" };
static struct item items[5] = { { 'a', 3, 1, 100 }, { 'b', -4, 2, 200 } };
static int grid[4][6];

static long sum_items(const struct item *it, int n) {
    long s = 0;
    for (const struct item *p = it; p < it + n; p++)
        s += p->tag * p->weight + p->n * p->id;
    return s;
}

int test_main(void) {
    for (int r = 0; r < 4; r++)
        for (int c = 0; c < 6; c++)
            grid[r][c] = r * 10 + c;
    items[2] = items[0];
    items[2].weight = 9;
    memcpy(&items[3], &items[1], sizeof items[1]);
    items[3].id += 5;
    char buf[16];
    memset(buf, 'x', sizeof buf);
    memcpy(buf + 3, names[1], 4);
    memmove(buf + 1, buf + 3, 5);
    int s = 0;
    for (int i = 0; i < 16; i++)
        s = s * 7 + buf[i];
    int *g = &grid[0][0];
    int *end = g + 24;
    long t = 0;
    while (end > g)
        t += *--end * (end - g);
    return (int)(sum_items(items, 5) + s + t + (long)strlen(names[0]) + names[2][3]);
}
