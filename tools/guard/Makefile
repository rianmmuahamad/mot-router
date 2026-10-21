CXX=/usr/bin/g++-10
CXXFLAGS=-std=c++17 -O2 -pthread -Wall
LDLIBS=-lcurl
SRC=$(wildcard src/*.cpp)
OBJ=$(SRC:.cpp=.o)

all: guard

.PHONY: all test clean

guard: $(OBJ)
	$(CXX) $(CXXFLAGS) -o $@ $(OBJ) $(LDLIBS)

%.o: %.cpp src/limit.h src/server.h
	$(CXX) $(CXXFLAGS) -c $< -o $@

tests/limit-test: tests/limit_test.cpp src/limit.cpp src/limit.h
	$(CXX) $(CXXFLAGS) -o $@ tests/limit_test.cpp src/limit.cpp

test: guard tests/limit-test
	./tests/limit-test
	python3 tests/integration.py ./guard

clean:
	rm -f $(OBJ) guard tests/limit-test
