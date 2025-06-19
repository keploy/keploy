# 🧠 Prompt: Optimizing Nested Loops in Java

You are given a nested loop structure that compares elements of an array with all other elements.

```java
for (int i = 0; i < arr.length; i++) {
    for (int j = 0; j < arr.length; j++) {
        if (arr[i] == arr[j] && i != j) {
            // do something
        }
    }
}
